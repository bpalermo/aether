// What the pinned proxy does when a pod's listeners and clusters are RENAMED
// in place, and with two pods of one name (issue #1584).
//
// Until #1584 the node agent named a pod's listeners and clusters after the
// pod's name alone (`inbound_web-0`, `app_web-0_8080`), so two pods of the
// same name in two namespaces on one node shared every one of them. The names
// now carry the namespace (`inbound_ns-a_web-0`, `app_ns-a_web-0_8080`). An
// agent upgraded under a running proxy therefore publishes, for every pod
// already on the node, a snapshot that REMOVES the listener under the old name
// and ADDS one under the new name at the same address in the same network
// namespace. This file measures what the proxy does with that update, and
// that two same-named pods are each answered by their own application.
//
// A pod's network namespace cannot be created in a hermetic test, so a pod is
// a loopback PORT here: "bound into the pod" is "bound to the port". The netns
// is part of the same SocketAddress, so the listener-manager path is the same
// one; that last step is an inference, not a measurement.
package mtlspool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// renameWait bounds each wait for the proxy to act on a published snapshot.
const renameWait = 30 * time.Second

// startNamedApp is one pod's application: it answers every request with its
// own name.
func startNamedApp(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, name)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// unusedPort is a loopback port nothing listens on, reserved for this test
// until it ends. The reservation is a socket that is bound but never listens,
// with SO_REUSEPORT: the proxy (whose listeners set SO_REUSEPORT too) can bind
// and listen on the port, a connection to it is refused until the proxy does,
// and no other process running next to this test is handed the port by the
// kernel in the meantime. A port that is merely found free and released can
// be taken by a parallel run before the proxy binds it.
func unusedPort(t *testing.T) int {
	t.Helper()
	const soReusePort = 0xf // SO_REUSEPORT on Linux; the target is Linux only
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	require.NoError(t, syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1))
	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	sa, err := syscall.Getsockname(fd)
	require.NoError(t, err)
	return sa.(*syscall.SockaddrInet4).Port
}

// namedInbound is a pod's inbound listener reduced to what a name is about: a
// listener name, an address that stands for the pod's network namespace, and
// an HTTP connection manager whose route names the app cluster.
func namedInbound(name, appCluster string, port int) *listenerv3.Listener {
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  hcmv3.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name: name,
			// As the agent's inbound route does: the app cluster and the
			// listener arrive on one ADS stream in no fixed order, and a route
			// that validated its cluster would have the listener rejected
			// whenever the listener is handled first.
			ValidateClusters: wrapperspb.Bool(false),
			VirtualHosts: []*routev3.VirtualHost{{
				Name:    "all",
				Domains: []string{"*"},
				Routes: []*routev3.Route{{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: appCluster},
						Timeout:          durationpb.New(5 * time.Second),
					}},
				}},
			}},
		}},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		}},
	}
	return &listenerv3.Listener{
		Name:    name,
		Address: socketAddress("127.0.0.1", port),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}},
		}},
	}
}

// namedApp is a pod's app delivery cluster: a name and the pod's application
// as its one endpoint.
func namedApp(name, appAddr string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(time.Second),
		LoadAssignment:       staticEndpoint(name, appAddr),
	}
}

// answer is one HTTP exchange through a listener: the status and the body
// (the name of the application that answered, when one did).
type answer struct {
	status int
	body   string
}

func (a answer) String() string { return fmt.Sprintf("%d %q", a.status, a.body) }

// askFresh opens a NEW connection to the listener on port, sends one request
// and closes.
func askFresh(port int) (answer, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return answer{}, err
	}
	defer conn.Close()
	return httpExchange(conn, bufio.NewReader(conn), "GET / HTTP/1.1\r\nHost: web\r\nConnection: close\r\n\r\n")
}

// askOn sends one keep-alive request on an established connection.
func askOn(conn net.Conn, r *bufio.Reader) (answer, error) {
	return httpExchange(conn, r, "GET / HTTP/1.1\r\nHost: web\r\n\r\n")
}

func httpExchange(conn net.Conn, r *bufio.Reader, request string) (answer, error) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(conn, request); err != nil {
		return answer{}, err
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, body: string(body)}, err
}

func awaitRename(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(renameWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("did not see: %s within %s", what, renameWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pinnedEnvoy locates the pinned proxy or skips the test.
func pinnedEnvoy(t *testing.T) string {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}
	return bin
}

func listenersAndClusters(ls []*listenerv3.Listener, cs []*clusterv3.Cluster) map[resourcev3.Type][]types.Resource {
	out := map[resourcev3.Type][]types.Resource{resourcev3.ListenerType: {}, resourcev3.ClusterType: {}}
	for _, l := range ls {
		out[resourcev3.ListenerType] = append(out[resourcev3.ListenerType], l)
	}
	for _, c := range cs {
		out[resourcev3.ClusterType] = append(out[resourcev3.ClusterType], c)
	}
	return out
}

// freshDialer opens a new connection to port every few milliseconds until
// stopped and records what each one got.
type freshDialer struct {
	mu      sync.Mutex
	total   int
	refused int
	other   map[string]int // any outcome that is not "200 <want>"
	stop    chan struct{}
	done    chan struct{}
}

func startFreshDialer(port int, want string) *freshDialer {
	d := &freshDialer{other: map[string]int{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for {
			select {
			case <-d.stop:
				return
			default:
			}
			a, err := askFresh(port)
			d.mu.Lock()
			d.total++
			switch {
			case err != nil && errors.Is(err, syscall.ECONNREFUSED):
				d.refused++
			case err != nil:
				d.other[err.Error()]++
			case a.status != http.StatusOK || a.body != want:
				d.other[a.String()]++
			}
			d.mu.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return d
}

func (d *freshDialer) finish() (total, refused int, other map[string]int) {
	close(d.stop)
	<-d.done
	return d.total, d.refused, d.other
}

// TestListenerRenamedAtTheSameAddress is the upgrade a running proxy sees when
// its agent starts naming a pod's listener and app cluster with the pod's
// namespace: ONE snapshot that removes `inbound_web-0` and `app_web-0_8080`
// and adds `inbound_ns-a_web-0` and `app_ns-a_web-0_8080`, the new listener at
// the address the old one holds.
//
// The gate (the test fails if any of it is not so):
//
//   - The proxy accepts the update: nothing rejected over LDS or CDS.
//   - It ends with one active listener, the new one, at the same address.
//   - A connection opened after the swap is answered by the pod's application.
//
// Reported in the test log, because it is what an operator sees on upgrade:
// what connections opened DURING the swap got, and what the connection that
// was established before it gets afterwards and when the proxy closes it.
//
// Two shapes, because a removed listener drains and its connection manager
// resolves the route's cluster BY NAME for every request:
//
//   - the INBOUND listener routes to the pod's app cluster, which is renamed in
//     the same snapshot: the old name is gone while the old listener drains;
//   - the OUTBOUND and capture listeners route to mesh service clusters, whose
//     names do not change.
func TestListenerRenamedAtTheSameAddress(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "web-0", Namespace: "ns-a", ServiceAccount: "web"}
	t.Run("the cluster it routes to is renamed with it (inbound)", func(t *testing.T) {
		newCluster := proxy.AppClusterName(pod, 8080)
		require.Equal(t, "app_ns-a_web-0_8080", newCluster)
		seen := renameListenerAtTheSameAddress(t, "inbound_web-0", proxy.InboundListenerName(pod), "app_web-0_8080", newCluster)
		// The cluster and the listener updates are two responses on the
		// stream, in no fixed order, so one more 200 right after the swap is
		// possible. What the connection is left with until its close is 503.
		require.GreaterOrEqual(t, len(seen), 2)
		assert.Equal(t, `503 ""`, seen[len(seen)-2],
			"the old listener's connection is not served once the old cluster name is gone; if it now is, the upgrade note in docs/runbook.md (chart 2.5.0) overstates the cost")
	})
	t.Run("the cluster it routes to keeps its name (outbound, capture)", func(t *testing.T) {
		const service = "echo.demo.mesh.internal"
		seen := renameListenerAtTheSameAddress(t, "outbound_http_web-0", proxy.OutboundListenerName(pod), service, service)
		require.NotEmpty(t, seen)
		for _, line := range seen[:len(seen)-1] {
			assert.Equal(t, `200 "ns-a/web-0"`, line, "until the proxy closes it, the old listener's connection is served as before")
		}
	})
}

// renameListenerAtTheSameAddress serves one listener under oldListener, swaps
// it for newListener at the same address in one snapshot (and oldCluster for
// newCluster, when they differ), checks the gate, and returns the distinct
// answers the connection established before the swap got, in order, ending
// with its close.
func renameListenerAtTheSameAddress(t *testing.T, oldListener, newListener, oldCluster, newCluster string) []string {
	t.Helper()
	const (
		drain = 3 * time.Second
		who   = "ns-a/web-0"
	)
	bin := pinnedEnvoy(t)
	require.NotEqual(t, oldListener, newListener)
	app := startNamedApp(t, who)
	port := unusedPort(t)

	cp := startADSControlPlane(t, listenersAndClusters(
		[]*listenerv3.Listener{namedInbound(oldListener, oldCluster, port)},
		[]*clusterv3.Cluster{namedApp(oldCluster, app)}))
	e := launchEnvoyOverADS(t, bin, cp, "--drain-time-s", strconv.Itoa(int(drain.Seconds())))
	h := &adsProxyHandle{adminAddr: e.admin, cp: cp}
	awaitRename(t, "the listener serving the pod's app under the old name", func() bool {
		a, _ := askFresh(port)
		return a.body == who
	})
	require.Equal(t, 1, h.statInt(t, "listener_manager.listener_added"))

	// A caller's keep-alive connection, established and used before the swap.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	r := bufio.NewReader(conn)
	a, err := askOn(conn, r)
	require.NoError(t, err)
	require.Equal(t, answer{http.StatusOK, who}, a)

	dialer := startFreshDialer(port, who)
	time.Sleep(200 * time.Millisecond) // a baseline of connections before the swap

	swapped := time.Now()
	cp.publish(t, "2", listenersAndClusters(
		[]*listenerv3.Listener{namedInbound(newListener, newCluster, port)},
		[]*clusterv3.Cluster{namedApp(newCluster, app)}))
	awaitRename(t, "the proxy adding the listener under the new name", func() bool {
		return h.statInt(t, "listener_manager.listener_added") == 2
	})
	t.Logf("the renamed listener was added within %s of the publish", time.Since(swapped).Round(time.Millisecond))

	// The established connection: one request every 100 ms until the proxy
	// closes it.
	var seen []string
	var closedAfter time.Duration
	awaitRename(t, "the proxy closing the connection the old listener accepted", func() bool {
		a, err := askOn(conn, r)
		if err != nil {
			closedAfter = time.Since(swapped)
			seen = append(seen, fmt.Sprintf("closed after %s (%v)", closedAfter.Round(100*time.Millisecond), err))
			return true
		}
		line := a.String()
		if n := len(seen); n == 0 || seen[n-1] != line {
			seen = append(seen, line)
		}
		time.Sleep(100 * time.Millisecond)
		return false
	})
	total, refused, other := dialer.finish()
	t.Logf("ESTABLISHED connection after the swap, distinct answers in order: %v", seen)
	t.Logf("NEW connections across the swap: %d opened, %d refused, other than \"200 %s\": %v", total, refused, who, other)

	// The gate.
	assert.Zero(t, h.statInt(t, "listener_manager.lds.update_rejected"), "the proxy rejected the listener update")
	assert.Zero(t, h.statInt(t, "cluster_manager.cds.update_rejected"), "the proxy rejected the cluster update")
	assert.Equal(t, 1, h.statInt(t, "listener_manager.listener_removed"), "the old name was removed")
	assert.Zero(t, h.statInt(t, "listener_manager.listener_create_failure"))
	awaitRename(t, "one active listener and none draining", func() bool {
		return h.statInt(t, "listener_manager.total_listeners_active") == 1 &&
			h.statInt(t, "listener_manager.total_listeners_draining") == 0
	})
	ls, err := e.listeners()
	require.NoError(t, err)
	var names []string
	for _, l := range ls {
		names = append(names, l.GetName())
		assert.Equal(t, uint32(port), l.GetLocalAddress().GetSocketAddress().GetPortValue())
	}
	assert.Equal(t, []string{newListener}, names, "the proxy holds the renamed listener, at the same address")
	a, err = askFresh(port)
	require.NoError(t, err, "a new connection after the swap")
	assert.Equal(t, answer{http.StatusOK, who}, a)

	// What the swap may cost, bounded so a regression in the proxy shows.
	assert.Zero(t, refused, "no new connection is refused while the listener is renamed")
	assert.LessOrEqual(t, closedAfter, drain+2*time.Second, "the old listener's connection is closed by the end of the drain time")
	return seen
}

// TestSameNamedPodsAreEachAnsweredByTheirOwnApplication publishes ns-a/web-0
// and ns-b/web-0 to the pinned proxy under the names the agent's generators
// give them, each inbound listener at its own address (its own network
// namespace) routing to its own app cluster, and asks each listener who
// answers.
//
// Before #1584 the two pods' listeners had one name and so had their app
// clusters. go-control-plane kept one of each, so the proxy held ONE listener
// (the other pod's address refused connections) and, when the listener it kept
// was one pod's and the cluster the other's, delivered that pod's connections
// to the other pod's application. The snapshot is republished with the two
// pods in every order: the order a snapshot lists them in decides nothing.
func TestSameNamedPodsAreEachAnsweredByTheirOwnApplication(t *testing.T) {
	bin := pinnedEnvoy(t)

	podA := &cniv1.CNIPod{Name: "web-0", Namespace: "ns-a", ServiceAccount: "web"}
	podB := &cniv1.CNIPod{Name: "web-0", Namespace: "ns-b", ServiceAccount: "web"}
	appA, appB := startNamedApp(t, "ns-a/web-0"), startNamedApp(t, "ns-b/web-0")
	portA, portB := unusedPort(t), unusedPort(t)
	require.NotEqual(t, portA, portB)

	inbound := func(pod *cniv1.CNIPod, port int) *listenerv3.Listener {
		return namedInbound(proxy.InboundListenerName(pod), proxy.AppClusterName(pod, 8080), port)
	}
	app := func(pod *cniv1.CNIPod, addr string) *clusterv3.Cluster {
		return namedApp(proxy.AppClusterName(pod, 8080), addr)
	}
	// The orders a map walk can hand the pods to a snapshot in. With one name
	// for both pods the last resource of a name won, so the order decided
	// which pod was served; and listeners and clusters were ordered
	// independently, which is how one pod's listener met the other's cluster.
	lAB := []*listenerv3.Listener{inbound(podA, portA), inbound(podB, portB)}
	lBA := []*listenerv3.Listener{inbound(podB, portB), inbound(podA, portA)}
	cAB := []*clusterv3.Cluster{app(podA, appA), app(podB, appB)}
	cBA := []*clusterv3.Cluster{app(podB, appB), app(podA, appA)}
	orders := []map[resourcev3.Type][]types.Resource{
		listenersAndClusters(lAB, cAB),
		listenersAndClusters(lBA, cAB),
		listenersAndClusters(lAB, cBA),
		listenersAndClusters(lBA, cBA),
	}

	cp := startADSControlPlane(t, orders[0])
	e := launchEnvoyOverADS(t, bin, cp)
	h := &adsProxyHandle{adminAddr: e.admin, cp: cp}

	for i, resources := range orders {
		if i > 0 {
			cp.publish(t, strconv.Itoa(i+1), resources)
			// With a name per pod nothing changed for the proxy, so there is
			// nothing to wait for but time: give an update that should not
			// exist the chance to arrive.
			time.Sleep(300 * time.Millisecond)
		}
		awaitRename(t, fmt.Sprintf("order %d: a listener serving at each pod's address", i), func() bool {
			a, errA := askFresh(portA)
			b, errB := askFresh(portB)
			return errA == nil && errB == nil && a.status == http.StatusOK && b.status == http.StatusOK
		})
		for range 20 {
			a, err := askFresh(portA)
			require.NoErrorf(t, err, "order %d: ns-a/web-0's listener", i)
			require.Equalf(t, answer{http.StatusOK, "ns-a/web-0"}, a, "order %d: ns-a/web-0's listener is answered by its own application", i)
			b, err := askFresh(portB)
			require.NoErrorf(t, err, "order %d: ns-b/web-0's listener", i)
			require.Equalf(t, answer{http.StatusOK, "ns-b/web-0"}, b, "order %d: ns-b/web-0's listener is answered by its own application", i)
		}
	}

	assert.Equal(t, 2, h.statInt(t, "listener_manager.listener_added"))
	assert.Zero(t, h.statInt(t, "listener_manager.listener_modified"), "no republication moved a listener")
	assert.Zero(t, h.statInt(t, "listener_manager.listener_removed"))
	assert.Equal(t, 2, h.statInt(t, "listener_manager.total_listeners_active"))
	assert.Zero(t, h.statInt(t, "listener_manager.lds.update_rejected"))
	assert.Zero(t, h.statInt(t, "cluster_manager.cds.update_rejected"))
}
