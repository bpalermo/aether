package mtlspool

// A short name ("multi", "multi:8080") resolves in the CALLER's namespace, on
// the pinned proxy, with the listeners and the tables the agent's cache
// generates: nothing here is hand-written but the requests.
//
// The rig's node knows two services named "multi":
//
//	demo/multi      a service-wide filter (it stamps x-chain-filter) and an
//	                HTTPRoute rule (it stamps x-route-rule)
//	elsewhere/multi neither
//
// so the two response headers say which of the two a request was answered
// as. The pod whose listeners are served is in ppCallerNamespace.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"aethermesh.dev/agent/internal/xds/proxy"
)

type ppShort struct {
	status int
	filter bool
	rule   bool
	reason string
	host   string // the Host the backend received
	err    error
}

func (r ppShort) String() string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	return fmt.Sprintf("status=%d x-aether-error=%q chain-filter-ran=%v route-rule-applied=%v backend-saw-host=%q", r.status, r.reason, r.filter, r.rule, r.host)
}

// ppGetHeaders is ppGet with request headers of the caller's choosing.
func ppGetHeaders(addr, host string, headers map[string]string) ppShort {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ppShort{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := ppShort{status: resp.StatusCode, filter: resp.Header.Get(ppHeader) != "", rule: resp.Header.Get(ppRuleHeader) != "", reason: resp.Header.Get("x-aether-error")}
	if resp.StatusCode == 200 {
		out.host = strings.TrimSpace(string(body))
	}
	return out
}

// ppSpoof is what a client would send to choose its own namespace if the
// mechanism were a header: it must change nothing.
var ppSpoof = map[string]string{
	"x-aether-caller-namespace":         "demo",
	proxy.CallerNamespaceFilterStateKey: "demo",
}

// ppStartFrom starts the rig for a pod in ns, with both listeners (the
// capture chain of port 8080 and the proxy port) and the registry answering.
func ppStartFrom(t *testing.T, ns string) (r *ppRig, capture, proxyPort string) {
	t.Helper()
	prev := ppCallerNamespace
	ppCallerNamespace = ns
	t.Cleanup(func() { ppCallerNamespace = prev })
	r = ppStart(t, ppChain(t))
	r.reg.armed.Store(true)
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	proxyPort = fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, proxyPort)
	time.Sleep(3 * time.Second) // let CDS and RDS land
	return r, r.addr, proxyPort
}

// TestPerPort_LiveShortNameResolvesInTheCallersNamespace: on both paths, from
// a pod in each of the two namespaces that have a "multi" and from one in a
// namespace that has none.
func TestPerPort_LiveShortNameResolvesInTheCallersNamespace(t *testing.T) {
	for _, tc := range []struct {
		ns     string
		status int
		// filter and rule: the service-wide filter and the HTTPRoute rule of
		// demo/multi ran. Both true is "answered as demo/multi"; both false
		// with a 200 is "answered as elsewhere/multi".
		filter, rule bool
		reason       string
	}{
		{ns: "demo", status: 200, filter: true, rule: true},
		{ns: "elsewhere", status: 200},
		{ns: "third", status: 421, reason: proxy.RefusalNameNotInNamespace},
	} {
		t.Run("caller in "+tc.ns, func(t *testing.T) {
			_, capture, proxyPort := ppStartFrom(t, tc.ns)
			for _, at := range []struct{ path, addr string }{{"captured dial to :8080", capture}, {"proxy port", proxyPort}} {
				for _, host := range []string{"multi:8080", "multi:9090"} {
					if at.addr == capture && host != "multi:8080" {
						continue // that chain is the dialled port 8080
					}
					got := ppGetHeaders(at.addr, host, ppSpoof)
					t.Logf("MEASURED caller in %-9s %-22s Host %-10s -> %s", tc.ns, at.path, host, got)
					if got.status != tc.status || got.filter != tc.filter || got.rule != tc.rule || got.reason != tc.reason {
						t.Errorf("caller in %s, %s, Host %s: want status %d filter=%v rule=%v reason=%q, got %s", tc.ns, at.path, host, tc.status, tc.filter, tc.rule, tc.reason, got)
					}
					// The application sees the Host the client sent.
					if got.status == 200 && got.host != host {
						t.Errorf("caller in %s, %s: the backend received Host %q, the client sent %q", tc.ns, at.path, got.host, host)
					}
				}
				// The full names are not the caller's business: each answers
				// as itself from anywhere.
				if got := ppGetHeaders(at.addr, ppMesh+":8080", nil); got.status != 200 || !got.filter || !got.rule {
					t.Errorf("caller in %s, %s: the full name of demo/multi: %s", tc.ns, at.path, got)
				}
				if got := ppGetHeaders(at.addr, "multi.elsewhere.aether.internal:8080", nil); got.status != 200 || got.filter || got.rule {
					t.Errorf("caller in %s, %s: the full name of elsewhere/multi: %s", tc.ns, at.path, got)
				}
			}
			// A name without a port is port 80, which neither declares: the
			// refusal of the caller's own service, not a delivery anywhere.
			if tc.status == 200 {
				got := ppGetHeaders(proxyPort, "multi", ppSpoof)
				t.Logf("MEASURED caller in %-9s proxy port             Host multi      -> %s", tc.ns, got)
				if got.status != 421 || got.reason != proxy.RefusalPortRequired {
					t.Errorf("caller in %s, proxy port, Host multi: want 421 port-required, got %s", tc.ns, got)
				}
			}
		})
	}
}

// TestPerPort_LiveShortNameWithoutACallerNamespaceIsRefused: a chain that
// does not store the caller's namespace refuses a short name. It does not fall
// through to another namespace's service, nor to the table's catch-all.
func TestPerPort_LiveShortNameWithoutACallerNamespaceIsRefused(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	r.mu.Lock()
	r.mutate = func(res map[resourcev3.Type][]types.Resource) {
		for i, lr := range res[resourcev3.ListenerType] {
			l := proto.Clone(lr).(*listenerv3.Listener)
			for _, fc := range l.GetFilterChains() {
				var kept []*listenerv3.Filter
				for _, f := range fc.GetFilters() {
					if !strings.Contains(f.String(), proxy.CallerNamespaceFilterStateKey) {
						kept = append(kept, f)
					}
				}
				require.Less(t, len(kept), len(fc.GetFilters()), "the chain carried no caller-namespace filter to remove")
				fc.Filters = kept
			}
			res[resourcev3.ListenerType][i] = l
		}
	}
	r.mu.Unlock()
	r.publish()
	time.Sleep(3 * time.Second)
	for _, at := range []struct{ path, addr string }{{"captured dial to :8080", capture}, {"proxy port", proxyPort}} {
		got := ppGetHeaders(at.addr, "multi:8080", ppSpoof)
		t.Logf("MEASURED no caller namespace, %-22s Host multi:8080 -> %s", at.path, got)
		if got.status != 421 || got.reason != proxy.RefusalNameNotInNamespace {
			t.Errorf("%s, no caller namespace on the connection: want 421 %s, got %s", at.path, proxy.RefusalNameNotInNamespace, got)
		}
		// The full name still works: only the short name needs the namespace.
		if got := ppGetHeaders(at.addr, ppMesh+":8080", nil); got.status != 200 || !got.filter {
			t.Errorf("%s: the full name: %s", at.path, got)
		}
	}
}

// TestPerPort_LiveColdRequestIsRefusedLikeAWarmOne: the node holds NO endpoint
// of demo/multi (the registry does not answer), and knows its ports from its
// generated Service: 8080 and 9090. What a client gets for the authorities
// that name no port of it, measured: the same 421 and reason as once the node
// holds an endpoint, at once, and nothing reaches a backend.
func TestPerPort_LiveColdRequestIsRefusedLikeAWarmOne(t *testing.T) {
	r := ppStart(t, ppChain(t)) // the registry is NOT armed: demo/multi has no endpoint here
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	proxyPort := fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, proxyPort)
	time.Sleep(3 * time.Second)

	before := r.hits.Load()
	for _, tc := range []struct{ host, reason string }{
		{ppMesh, proxy.RefusalPortRequired},               // a name without a port is port 80
		{ppMesh + ":80", proxy.RefusalPortNotDeclared},    // which it does not declare
		{ppMesh + ":18081", proxy.RefusalPortNotDeclared}, // the proxy's port is no service's
		{ppMesh + ":7777", proxy.RefusalPortNotDeclared},
		{"multi", proxy.RefusalPortRequired}, // the short name, as the full one
		{"multi:18081", proxy.RefusalPortNotDeclared},
	} {
		start := time.Now()
		got := ppGetHeaders(proxyPort, tc.host, nil)
		t.Logf("MEASURED cold, proxy port, Host %-34s -> %s in %s", tc.host, got, time.Since(start).Round(time.Millisecond))
		if got.status != 421 || got.reason != tc.reason {
			t.Errorf("cold, Host %s: want 421 %s, got %s", tc.host, tc.reason, got)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("cold, Host %s: the refusal took %s; it must not wait for a cluster", tc.host, time.Since(start))
		}
	}
	if after := r.hits.Load(); after != before {
		t.Errorf("a cold request that names no port of the service reached a backend (%d hits)", after-before)
	}
}

// TestPerPort_LiveColdRequestOfANameTheAgentKnowsNothingOf: the generated
// Services carry no ports (the agent knows the names only) and the registry
// does not answer. Measured, on the proxy port:
//
//   - a name without a port is not offered on demand: it fails at once, and
//     reaches no backend;
//   - a name with a port is offered on demand; what the client gets when no
//     cluster of that name ever arrives is recorded.
func TestPerPort_LiveColdRequestOfANameTheAgentKnowsNothingOf(t *testing.T) {
	prev := ppDeclared
	ppDeclared = false
	t.Cleanup(func() { ppDeclared = prev })
	r := ppStart(t, ppChain(t))
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	proxyPort := fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, proxyPort)
	time.Sleep(3 * time.Second)

	before := r.hits.Load()
	// demo/multi carries a service-wide filter, so it has a stand-in virtual
	// host: its port-less authority is refused by name. A name with nothing
	// at all (no filter, no route, no ports) falls to the table's floor.
	for _, tc := range []struct {
		host   string
		status int
		reason string
	}{
		{ppMesh, 421, proxy.RefusalPortRequired},
		{"nothing.demo.aether.internal", 404, ""},
	} {
		start := time.Now()
		got := ppGetHeaders(proxyPort, tc.host, nil)
		t.Logf("MEASURED unknown, proxy port, Host %-30s -> %s in %s", tc.host, got, time.Since(start).Round(time.Millisecond))
		if got.status != tc.status || got.reason != tc.reason {
			t.Errorf("unknown, Host %s: want %d %q, got %s", tc.host, tc.status, tc.reason, got)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("unknown, Host %s: took %s", tc.host, time.Since(start))
		}
	}
	// With a port: offered on demand. Nothing answers for the name here, so
	// this records what the client gets and how long it takes.
	start := time.Now()
	got := ppGetHeaders(proxyPort, "nothing.demo.aether.internal:7777", nil)
	t.Logf("MEASURED unknown, proxy port, Host nothing.demo.aether.internal:7777 (on demand, never answered) -> %s in %s", got, time.Since(start).Round(time.Millisecond))
	if got.status == 200 {
		t.Errorf("a name nothing serves was answered 200: %s", got)
	}
	if after := r.hits.Load(); after != before {
		t.Errorf("a request for a name the agent knows nothing of reached a backend (%d hits)", after-before)
	}
}

// TestPerPort_LiveProxyPortAuthorityIsNeverDelivered: "<name>:18081" names the
// mesh's HTTP proxy port, a port of no service, so it is never delivered. The
// case that used to be: a name the agent knows NOTHING of (no endpoint, no
// port on its generated Service), asked for as "<fqdn>:18081". It was offered
// on demand like any "<fqdn>:<port>" and parked; when the service entered the
// node's scope meanwhile, an alias cluster of that name answered, and the
// request reached the port the pods declare.
//
// Here the registry starts answering 300 ms after the request is sent, which
// is the park the old path needed. Measured: the request is refused at once
// and is not parked at all, nothing reaches a backend then or after the
// service is known, no cluster of that name is ever published, and the
// service's real port is delivered once it is known (so the rig can deliver).
func TestPerPort_LiveProxyPortAuthorityIsNeverDelivered(t *testing.T) {
	prev := ppDeclared
	ppDeclared = false
	t.Cleanup(func() { ppDeclared = prev })
	r := ppStart(t, ppChain(t))
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	var subscribed sync.Map
	r.onClusterSubscribe = func(name string) { subscribed.Store(name, true) }
	r.publish()
	proxyPort := fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, proxyPort)
	time.Sleep(3 * time.Second)

	load := func() {
		r.reg.armed.Store(true)
		require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
		r.publish()
	}
	// A name with a filter (a stand-in virtual host) and a name with nothing.
	hosts := []string{ppMesh + ":18081", "multi:18081", "nothing.demo.aether.internal:18081", ppMesh + ":18082"}

	before := r.hits.Load()
	entered := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		load()
		close(entered)
	}()
	for _, host := range hosts {
		start := time.Now()
		got := ppGetHeaders(proxyPort, host, nil)
		took := time.Since(start)
		t.Logf("MEASURED unknown name, Host %-36s while the service enters scope -> %s in %s", host, got, took.Round(time.Millisecond))
		if got.status != 421 || got.reason != proxy.RefusalPortNotDeclared {
			t.Errorf("Host %s: want 421 %s, got %s", host, proxy.RefusalPortNotDeclared, got)
		}
		if took > time.Second {
			t.Errorf("Host %s: answered after %s; it must not be parked", host, took)
		}
	}
	<-entered
	time.Sleep(2 * time.Second) // let CDS and RDS land

	// The service is known now. The spelling is still refused, by the
	// service's own virtual host.
	for _, host := range hosts {
		got := ppGetHeaders(proxyPort, host, nil)
		t.Logf("MEASURED known name,   Host %-36s -> %s", host, got)
		if got.status != 421 || got.reason != proxy.RefusalPortNotDeclared {
			t.Errorf("known, Host %s: want 421 %s, got %s", host, proxy.RefusalPortNotDeclared, got)
		}
	}
	if after := r.hits.Load(); after != before {
		t.Errorf("an authority that names the mesh's own port reached a backend (%d hits)", after-before)
	}
	subscribed.Range(func(k, _ any) bool {
		if name := k.(string); strings.HasSuffix(name, ":18081") || strings.HasSuffix(name, ":18082") {
			t.Errorf("the proxy asked for cluster %q on demand", name)
		}
		return true
	})
	snap, err := r.cp.cache.GetSnapshot(envoyNodeID)
	require.NoError(t, err)
	for name := range snap.GetResources(resourcev3.ClusterType) {
		if strings.HasSuffix(name, ":18081") || strings.HasSuffix(name, ":18082") {
			t.Errorf("cluster %q is published", name)
		}
	}
	// Anti-vacuity: the same rig delivers the service's real port.
	got := ppGetHeaders(proxyPort, ppMesh+":8080", nil)
	t.Logf("MEASURED known name,   Host %-36s -> %s", ppMesh+":8080", got)
	if got.status != 200 || r.hits.Load() == before {
		t.Errorf("the real port is not delivered either, so the refusals above prove nothing: %s", got)
	}
}
