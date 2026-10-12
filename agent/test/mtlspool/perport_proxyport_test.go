package mtlspool

// The loopback outbound listener as an HTTP proxy port: what the pinned proxy
// does with an absolute-form request line ("GET http://name:port/ HTTP/1.1",
// what a client configured with an HTTP proxy sends).
//
// The listener, its connection manager and the out_http table are the ones
// agent/internal/xds/cache.SnapshotCache generates; only the listener's
// address is moved to loopback outside a network namespace. The requests are
// written byte for byte, so the request line and the Host header are exactly
// what the test names.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// ppWithProxyPort adds the pod's loopback outbound listener to a translated
// snapshot, bound to 127.0.0.1:<port>.
func ppWithProxyPort(t *testing.T, c *cache.SnapshotCache, res map[resourcev3.Type][]types.Resource, port uint32) {
	t.Helper()
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "outbound_http_") {
			continue
		}
		l := proto.Clone(r).(*listenerv3.Listener)
		l.Address = &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}}
		l.Name = "proxyport"
		res[resourcev3.ListenerType] = append(res[resourcev3.ListenerType], l)
		return
	}
	t.Fatal("the snapshot has no outbound HTTP listener")
}

type ppRaw struct {
	status  int
	reason  string
	filter  bool
	seen    string // the Host the backend received
	closed  bool
	failure string
}

func (r ppRaw) String() string {
	if r.failure != "" {
		return "no response: " + r.failure
	}
	return fmt.Sprintf("status=%d x-aether-error=%q chain-filter-ran=%v backend-saw-host=%q", r.status, r.reason, r.filter, r.seen)
}

// ppSendRaw writes one HTTP/1.1 request with the given request target and
// Host header, and reads the response.
func ppSendRaw(addr, method, target, host string) ppRaw {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return ppRaw{failure: err.Error()}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	req := method + " " + target + " HTTP/1.1\r\n"
	if host != "" {
		req += "Host: " + host + "\r\n"
	}
	req += "User-Agent: probe\r\nAccept: */*\r\nProxy-Connection: Keep-Alive\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return ppRaw{failure: err.Error()}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return ppRaw{failure: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return ppRaw{status: resp.StatusCode, reason: resp.Header.Get("x-aether-error"), filter: resp.Header.Get(ppHeader) != "", seen: strings.TrimSpace(string(body))}
}

// ppAwaitListening waits until something accepts on addr.
func ppAwaitListening(t *testing.T, addr string) {
	t.Helper()
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 20*time.Second, 200*time.Millisecond, "the proxy never bound %s", addr)
}

// TestPerPort_ParkedRequestOnTheProxyPortRunsTheChainFilter: the loopback
// path's half of TestPerPort_ParkedRequestSkipsTheChainFilter. The node holds
// no endpoint of the service when the request arrives on the loopback
// listener; the on-demand filter parks it; the registry answers, the cluster
// is published and the request resumes. It must not be delivered without the
// service's chain filter (an ext_authz filter is one).
func TestPerPort_ParkedRequestOnTheProxyPortRunsTheChainFilter(t *testing.T) {
	r := ppStart(t, ppChain(t))
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, addr)
	time.Sleep(2 * time.Second) // let RDS land

	published := make(chan struct{})
	r.onClusterSubscribe = func(string) {
		// The registry becomes reachable while the request is parked.
		time.Sleep(300 * time.Millisecond)
		r.reg.armed.Store(true)
		require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
		r.publish()
		close(published)
	}
	host := ppMesh + ":8080"
	first := ppGet(addr, host)
	t.Logf("no endpoint held, loopback, Host %s (parked on ODCDS) -> %s", host, first)
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never asked for the cluster by name: the request was not parked, and the test saw nothing")
	}
	time.Sleep(2 * time.Second)
	second := ppGet(addr, host)
	t.Logf("endpoints held,   loopback, Host %s                   -> %s", host, second)
	if !second.filter {
		t.Errorf("control: the SECOND request did not run the chain filter either (%s); the harness cannot see the filter", second)
	}
	if first.status == 200 && !first.filter {
		t.Errorf("the parked request was delivered to the backend (200) WITHOUT the service's chain filter: %s", first)
	}
	// Nor without its HTTPRoute's rule: the request resumes on a route of the
	// service's own virtual host, the one the rule put there.
	if !second.rule {
		t.Errorf("control: the SECOND request did not carry the route rule's header either (%s)", second)
	}
	if first.status == 200 && !first.rule {
		t.Errorf("the parked request was delivered to the backend (200) with the service's HTTPRoute rule SKIPPED: %s", first)
	}
	// A rule whose backendRef names no port of its Service answers 500 and
	// reaches no backend, with the cluster held.
	before := r.hits.Load()
	invalid := ppGetPath(addr, host, ppInvalidPath)
	t.Logf("a rule with an invalid backendRef                      -> %s", invalid)
	if invalid.status != 500 || r.hits.Load() != before {
		t.Errorf("an invalid backendRef: want 500 and no backend hit, got %s (%d hits)", invalid, r.hits.Load()-before)
	}
}

// TestPerPort_ProxyPortAbsoluteForm measures, on the pinned proxy, what the
// loopback outbound listener does with an absolute-form request line.
//
// Service multi carries a chain filter that stamps a response header; service
// other carries none. Whether the header is on the response says which of the
// two the request was routed as. The backend echoes the Host it received.
func TestPerPort_ProxyPortAbsoluteForm(t *testing.T) {
	r := ppStart(t, ppChain(t))
	r.reg.armed.Store(true)
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 20*time.Second, 200*time.Millisecond, "the proxy never bound the proxy-port listener")
	time.Sleep(2 * time.Second) // let RDS and CDS land

	const other = "other.demo.aether.internal"
	for _, tc := range []struct {
		name, method, target, host string
	}{
		{"origin-form, Host multi:8080 (control)", "GET", "/", ppMesh + ":8080"},
		{"origin-form, Host other:8080 (control)", "GET", "/", other + ":8080"},
		{"absolute-form multi:8080, Host the same (curl --proxy)", "GET", "http://" + ppMesh + ":8080/", ppMesh + ":8080"},
		{"absolute-form multi:8080, Host other:8080", "GET", "http://" + ppMesh + ":8080/", other + ":8080"},
		{"absolute-form other:8080, Host multi:8080", "GET", "http://" + other + ":8080/", ppMesh + ":8080"},
		{"absolute-form multi:8080, no Host", "GET", "http://" + ppMesh + ":8080/", ""},
		{"absolute-form multi (port-less), Host port-less (curl --proxy http://name:80/)", "GET", "http://" + ppMesh + "/", ppMesh},
		{"absolute-form multi:80, Host multi:80 (--request-target)", "GET", "http://" + ppMesh + ":80/", ppMesh + ":80"},
		{"absolute-form multi:7777 (undeclared), Host the same", "GET", "http://" + ppMesh + ":7777/", ppMesh + ":7777"},
	} {
		got := ppSendRaw(addr, tc.method, tc.target, tc.host)
		t.Logf("MEASURED %-78s -> %s", tc.name, got)
	}

	// The request line's authority decides, not the Host header: the backend
	// receives it as Host, and the service it names is the one whose filter
	// runs.
	for _, tc := range []struct {
		target, host, want string
		routedAsMulti      bool
	}{
		{"http://" + ppMesh + ":8080/", ppMesh + ":8080", ppMesh + ":8080", true},
		{"http://" + ppMesh + ":8080/", other + ":8080", ppMesh + ":8080", true},
		{"http://" + other + ":8080/", ppMesh + ":8080", other + ":8080", false},
		{"http://" + ppMesh + ":8080/", "", ppMesh + ":8080", true},
	} {
		got := ppSendRaw(addr, "GET", tc.target, tc.host)
		if got.status != 200 || got.seen != tc.want || got.filter != tc.routedAsMulti {
			t.Errorf("GET %s with Host %q: want 200, the backend seeing Host %q, routed as multi: %v; got %s", tc.target, tc.host, tc.want, tc.routedAsMulti, got)
		}
	}
	// An undeclared port is refused by name, never delivered to another port.
	if got := ppSendRaw(addr, "GET", "http://"+ppMesh+":7777/", ppMesh+":7777"); got.status != 421 || got.reason != "port-not-declared" {
		t.Errorf("an undeclared port through the proxy port: want 421 port-not-declared, got %s", got)
	}
	// A name outside the mesh, and CONNECT (what a client sends for https://
	// through an HTTP proxy), reach no backend: the proxy port carries HTTP to
	// mesh names only.
	before := r.hits.Load()
	for _, tc := range [][3]string{
		{"GET", "http://example.invalid:8080/", "example.invalid:8080"},
		{"CONNECT", ppMesh + ":8080", ppMesh + ":8080"},
	} {
		got := ppSendRaw(addr, tc[0], tc[1], tc[2])
		t.Logf("MEASURED %-78s -> %s", tc[0]+" "+tc[1], got)
		if got.status == 200 {
			t.Errorf("%s %s was answered 200", tc[0], tc[1])
		}
	}
	if after := r.hits.Load(); after != before {
		t.Errorf("a request the proxy port does not carry reached a backend (%d hits)", after-before)
	}
}
