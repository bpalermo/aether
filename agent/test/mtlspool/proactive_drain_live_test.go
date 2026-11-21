package mtlspool

// Upstream Envoy's proactive idle-connection drain (upstream Envoy PR 47991),
// against the pinned proxy binary. When a listener drains, every HTTP
// connection manager arms a timer at a random point of
// [drain_time*2/3, drain_time - drain_timeout] that starts the drain sequence
// (an HTTP/2 GOAWAY first) on the connection, idle or not, and arms nothing
// when that window is empty. The node proxy drains for --drain-time-s 10, so
// with Envoy's default 5s drain_timeout the window is [6.67s, 5s] and an idle
// connection gets no GOAWAY at all; with the agent's 2s
// (proxy.DefaultConnectionDrainTimeout) it is [6.67s, 8s].
//
// The connection is HTTP/2, not HTTP/3, on purpose. The mechanism is
// protocol-agnostic (it runs in the connection manager), and the pinned binary
// still carries the aether1054 patch until the proxy pin moves past its removal:
// that patch arms its own HTTP/3-only timer over [0, drain_time -
// drain_timeout], which would fire inside both assertions below. HTTP/2 sees
// upstream's behaviour alone, on the patched and the unpatched build alike.
//
// The drain is POST /drain_listeners?graceful: it notifies every connection
// with the same ConnectionDrainEvent (start time, --drain-strategy) a
// hot-restart parent's drainListeners() does.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// proactiveDrainTime is the node proxy's --drain-time-s (the chart's
// proxy.hotRestart.drainTime).
const proactiveDrainTime = 10 * time.Second

// drainProbeListener is a plain-TCP listener whose one HCM answers every
// request locally and carries drainTimeout as its drain_timeout.
func drainProbeListener(t *testing.T, drainTimeout time.Duration) *listenerv3.Listener {
	t.Helper()
	router, err := anypb.New(&routerv3.Router{})
	if err != nil {
		t.Fatal(err)
	}
	hcm, err := anypb.New(&http_connection_managerv3.HttpConnectionManager{
		StatPrefix: "drain_probe",
		CodecType:  http_connection_managerv3.HttpConnectionManager_AUTO,
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
			IdleTimeout: durationpb.New(time.Hour),
		},
		DrainTimeout: durationpb.New(drainTimeout),
		HttpFilters: []*http_connection_managerv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{TypedConfig: router},
		}},
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: &routev3.RouteConfiguration{
				Name: "drain_probe",
				VirtualHosts: []*routev3.VirtualHost{{
					Name:    "all",
					Domains: []string{"*"},
					Routes: []*routev3.Route{{
						Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 200}},
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &listenerv3.Listener{
		Name:    "drain_probe",
		Address: socketAddress("127.0.0.1", envoyPicksPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcm},
			}},
		}},
	}
}

// h2Frame types this test reads.
const (
	h2FrameSettings = 0x4
	h2FrameGoAway   = 0x7
	h2FlagAck       = 0x1
)

// idleH2Conn is an HTTP/2 connection that has sent its preface and SETTINGS
// and nothing else: the connection manager has created its codec (so a drain
// sequence sends a GOAWAY rather than just closing) and holds no stream.
type idleH2Conn struct {
	conn net.Conn
	mu   sync.Mutex
	// goAway is when the first GOAWAY frame arrived; closed is when the
	// connection ended. Zero until they happen.
	goAway, closed time.Time
	done           chan struct{}
}

func dialIdleH2(t *testing.T, addr string) *idleH2Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var preface bytes.Buffer
	preface.WriteString("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	preface.Write([]byte{0, 0, 0, h2FrameSettings, 0, 0, 0, 0, 0}) // empty SETTINGS
	if _, err := conn.Write(preface.Bytes()); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	c := &idleH2Conn{conn: conn, done: make(chan struct{})}
	go c.read()
	return c
}

// read records the first GOAWAY and the end of the connection, acknowledging
// the server's SETTINGS so the connection is a well-behaved idle one.
func (c *idleH2Conn) read() {
	defer close(c.done)
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(c.conn, hdr); err != nil {
			c.mu.Lock()
			c.closed = time.Now()
			c.mu.Unlock()
			return
		}
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		typ, flags := hdr[3], hdr[4]
		if _, err := io.CopyN(io.Discard, c.conn, int64(length)); err != nil {
			c.mu.Lock()
			c.closed = time.Now()
			c.mu.Unlock()
			return
		}
		switch {
		case typ == h2FrameSettings && flags&h2FlagAck == 0:
			_, _ = c.conn.Write([]byte{0, 0, 0, h2FrameSettings, h2FlagAck, 0, 0, 0, 0})
		case typ == h2FrameGoAway:
			c.mu.Lock()
			if c.goAway.IsZero() {
				c.goAway = time.Now()
			}
			c.mu.Unlock()
		}
	}
}

func (c *idleH2Conn) events() (goAway, closed time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.goAway, c.closed
}

// startDrainProbe runs the pinned proxy with --drain-time-s 10 and the
// gradual strategy (the chart's values), opens one idle HTTP/2 connection to
// a listener whose HCM has drainTimeout, starts a graceful listener drain and
// returns the connection and the drain's start.
func startDrainProbe(t *testing.T, drainTimeout time.Duration) (*idleH2Conn, time.Time) {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Listeners: []*listenerv3.Listener{drainProbeListener(t, drainTimeout)},
		},
	}
	e := launchEnvoy(t, bin, "drain", bs, nil,
		"--drain-time-s", "10", "--drain-strategy", "gradual", "--concurrency", "1")
	addr := e.listenerAddr(t, "drain_probe")

	c := dialIdleH2(t, addr)
	// Let the server's SETTINGS exchange settle so the codec exists and
	// nothing but the drain can produce a frame from here on.
	time.Sleep(300 * time.Millisecond)
	if g, cl := c.events(); !g.IsZero() || !cl.IsZero() {
		t.Fatalf("connection ended or got a GOAWAY before the drain (goaway %v, closed %v)", g, cl)
	}

	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Post("http://"+e.admin+"/drain_listeners?graceful", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /drain_listeners?graceful: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /drain_listeners?graceful: %s", resp.Status)
	}
	return c, start
}

// TestProactiveDrainGoAwaysAnIdleConnectionWithTheAgentsDrainTimeout: with
// drain_timeout 2s an idle connection receives its GOAWAY inside
// [6.67s, 8s] of the drain's start, and not before.
func TestProactiveDrainGoAwaysAnIdleConnectionWithTheAgentsDrainTimeout(t *testing.T) {
	t.Parallel()
	if proxy.DefaultConnectionDrainTimeout != 2*time.Second {
		t.Fatalf("DefaultConnectionDrainTimeout = %v; this test's window is computed for 2s", proxy.DefaultConnectionDrainTimeout)
	}
	c, start := startDrainProbe(t, proxy.DefaultConnectionDrainTimeout)

	const (
		earliest = 6500 * time.Millisecond               // window start 6.67s, minus slack
		latest   = 8*time.Second + 1500*time.Millisecond // window end 8s, plus slack
		windowLo = proactiveDrainTime * 2 / 3            // for the message
		windowHi = proactiveDrainTime - 2*time.Second    // for the message
	)
	select {
	case <-c.done:
	case <-time.After(time.Until(start.Add(latest))):
	}
	goAway, closed := c.events()
	if goAway.IsZero() {
		t.Fatalf("no GOAWAY within %v of the drain start (closed: %v): upstream's proactive drain did not arm for window [%v, %v]",
			latest, closed, windowLo, windowHi)
	}
	at := goAway.Sub(start)
	t.Logf("GOAWAY %v after the drain started (window [%v, %v])", at, windowLo, windowHi)
	if at < earliest {
		t.Errorf("GOAWAY %v after the drain started, before the window opens at %v", at, windowLo)
	}
	if at > latest {
		t.Errorf("GOAWAY %v after the drain started, after the window closes at %v", at, windowHi)
	}
	if !closed.IsZero() && closed.Before(goAway) {
		t.Errorf("connection closed (%v) before its GOAWAY (%v)", closed.Sub(start), at)
	}
}

// TestProactiveDrainArmsNothingWithEnvoysDefaultDrainTimeout is the control
// that gives the test above its teeth: with Envoy's 5s default the window is
// empty and the idle connection gets nothing before the drain time ends.
func TestProactiveDrainArmsNothingWithEnvoysDefaultDrainTimeout(t *testing.T) {
	t.Parallel()
	c, start := startDrainProbe(t, 5*time.Second)

	const watch = proactiveDrainTime - 300*time.Millisecond
	select {
	case <-c.done:
	case <-time.After(time.Until(start.Add(watch))):
	}
	goAway, closed := c.events()
	if !goAway.IsZero() {
		t.Errorf("GOAWAY %v after the drain started with drain_timeout 5s: the window [6.67s, 5s] is empty and should arm nothing",
			goAway.Sub(start))
	}
	if !closed.IsZero() {
		t.Errorf("connection closed %v after the drain started with drain_timeout 5s, before the drain time ended", closed.Sub(start))
	}
}
