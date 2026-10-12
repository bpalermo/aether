package mtlspool

// Third adversarial pass on the proposal-043 stack (head 7f0f8299): review
// tests on the pinned proxy, uncommitted. They use the stack's own rig
// (perport_live_test.go): the listeners and route tables are the ones the
// agent's cache generates.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"

	"aethermesh.dev/agent/internal/xds/proxy"
)

const r3RouteExtHeader = "x-route-ext"

type r3Resp struct {
	status          int
	chain, routeExt bool
	rule            bool
	reason          string
	took            time.Duration
	err             error
}

func (r r3Resp) String() string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	return fmt.Sprintf("status=%d x-aether-error=%q service-wide-filter-ran=%v route-filter-ran=%v took=%s", r.status, r.reason, r.chain, r.routeExt, r.took.Round(time.Millisecond))
}

func r3Get(addr, host, path string) r3Resp {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	req.Host = host
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return r3Resp{err: err, took: time.Since(start)}
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return r3Resp{
		status: resp.StatusCode, chain: resp.Header.Get(ppHeader) != "", routeExt: resp.Header.Get(r3RouteExtHeader) != "",
		rule: resp.Header.Get(ppRuleHeader) != "", reason: resp.Header.Get("x-aether-error"), took: time.Since(start),
	}
}

func r3ProxyPort(t *testing.T, r *ppRig) string {
	t.Helper()
	port := ppFreePort(t)
	r.mu.Lock()
	r.proxyPort = port
	r.mu.Unlock()
	r.publish()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ppAwaitListening(t, addr)
	time.Sleep(3 * time.Second)
	return addr
}

// TestPerPort_LiveShortNameOfAServiceKnownByNameOnly: the generated Services
// carry no real ports (the agent knows the names only) and the node holds no
// endpoint of demo/multi, which carries a service-wide filter. On the proxy
// port, from a pod in "demo":
//
//	Host multi.demo.aether.internal:8080  parked, the cluster is fetched, delivered
//	Host multi:8080                       must be the same request
//
// The rig's control plane answers an on-demand request the way the agent's
// observer does (agent/internal/xds/server/odcds.go, observeSubscription): a
// name that is not a mesh authority is dropped.
func TestPerPort_LiveShortNameOfAServiceKnownByNameOnly(t *testing.T) {
	prev := ppDeclared
	ppDeclared = false
	t.Cleanup(func() { ppDeclared = prev })
	r := ppStart(t, ppChain(t))
	var mu sync.Mutex
	var asked []string
	r.onClusterSubscribe = func(name string) {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
		if _, ok := proxy.ServiceFromClusterName(name, "aether.internal"); !ok {
			t.Logf("[agent] on-demand request for %q: not a mesh authority, dropped (odcds.go observeSubscription)", name)
			return
		}
		time.Sleep(200 * time.Millisecond)
		r.reg.armed.Store(true)
		require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
		r.publish()
	}
	addr := r3ProxyPort(t, r)

	short := r3Get(addr, "multi:8080", "/")
	mu.Lock()
	t.Logf("MEASURED cold, proxy port, caller in demo, Host multi:8080 -> %s; the proxy asked for clusters %q", short, asked)
	askedShort := append([]string(nil), asked...)
	mu.Unlock()
	full := r3Get(addr, ppMesh+":8080", "/")
	mu.Lock()
	t.Logf("MEASURED cold, proxy port, Host %s:8080 -> %s; the proxy asked for clusters %q", ppMesh, full, asked)
	mu.Unlock()
	time.Sleep(3 * time.Second)
	warm := r3Get(addr, "multi:8080", "/")
	t.Logf("MEASURED warm, proxy port, caller in demo, Host multi:8080 -> %s", warm)

	require.Equal(t, 200, full.status, "the full name is parked and delivered")
	require.Equal(t, 200, warm.status, "the short name is delivered once the node holds the service")
	if short.status != full.status {
		t.Errorf("cold, Host multi:8080: %s; the full name of the same service got %s. The proxy asked for %q", short, full, askedShort)
	}
}

// TestPerPort_LiveServiceWideFilterAndARouteFilterOfTheSameKind: demo/multi
// has a service-wide header_mutation filter (it stamps x-chain-filter) and an
// HTTPRoute rule whose ExtensionRef is ALSO a header_mutation (it stamps
// x-route-ext). The node holds the service. What runs under the full name
// must run under the short name.
func TestPerPort_LiveServiceWideFilterAndARouteFilterOfTheSameKind(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	perRoute, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: r3RouteExtHeader, Value: "ran"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}},
	}})
	require.NoError(t, err)
	r.c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {{
		Matches:          []proxy.GammaMatch{{Prefix: "/"}},
		ExtensionFilters: []proxy.ExtensionFilter{{Name: "envoy.filters.http.header_mutation", Config: perRoute}},
	}}})
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	r.publish()
	time.Sleep(3 * time.Second)

	for _, at := range []struct{ path, addr string }{{"captured dial to :8080", capture}, {"proxy port", proxyPort}} {
		full := r3Get(at.addr, ppMesh+":8080", "/")
		short := r3Get(at.addr, "multi:8080", "/")
		t.Logf("MEASURED %-22s Host %s:8080 -> %s", at.path, ppMesh, full)
		t.Logf("MEASURED %-22s Host multi:8080 -> %s", at.path, short)
		require.Equal(t, 200, full.status)
		require.Equal(t, 200, short.status)
		require.True(t, full.routeExt, "the route's own filter runs under the full name")
		if full.chain != short.chain {
			t.Errorf("%s: the service-wide filter ran=%v under the full name and ran=%v under the short name", at.path, full.chain, short.chain)
		}
		if full.routeExt != short.routeExt {
			t.Errorf("%s: the route's filter ran=%v under the full name and ran=%v under the short name", at.path, full.routeExt, short.routeExt)
		}
	}
}

// TestPerPort_LiveColdRuleBackendsAndAnUndeclaredPort: demo/multi has an
// HTTPRoute rule with a backendRef (to demo/other, port 8080, which the node
// holds), and the node holds no endpoint of demo/multi. On the proxy port,
// Host multi.demo.aether.internal:7777 names a port demo/multi does not have.
// Warm it is refused (421 port-not-declared). Cold:
//
//   - "a user's Service owns the name": the node has that Service's port map,
//     so it knows the ports. The request is refused as warm, at once, and the
//     rule's backend is not reached. This is a steady state on the proxy port
//     and the one the third review measured delivered (200).
//   - "known by name only" (a generated Service with no real port on it, as a
//     chart 2.6 registrar writes it): nothing on the node says what the ports
//     are, and the rule's backend still answers. A STATED LIMIT, asserted as
//     it is so that it cannot change unnoticed (runbook, "While a node holds
//     no endpoint of a service").
func TestPerPort_LiveColdRuleBackendsAndAnUndeclaredPort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		portMap map[uint32]uint32
		cold    int
	}{
		{"a user's Service owns the name", map[uint32]uint32{8080: 8080, 9090: 9090}, 421},
		{"known by name only", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := ppDeclared
			ppDeclared = false
			t.Cleanup(func() { ppDeclared = prev })
			r := ppStart(t, ppChain(t))
			if tc.portMap != nil {
				r.c.SetServicePortMaps(map[string]map[uint32]uint32{ppSvc: tc.portMap})
			}
			r.c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {{
				Matches:  []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{{Service: "demo/other", Cluster: "other.demo.aether.internal", Port: 8080, Weight: 1}},
			}}})
			require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
			addr := r3ProxyPort(t, r)

			host := ppMesh + ":7777"
			before := r.hits.Load()
			cold := r3Get(addr, host, "/")
			coldHits := r.hits.Load() - before
			t.Logf("MEASURED cold (%s), proxy port, Host %s -> %s; backend hits %d", tc.name, host, cold, coldHits)
			// Anti-vacuity: a port the service does have runs the rule, cold.
			declared := r3Get(addr, ppMesh+":8080", "/")
			t.Logf("MEASURED cold (%s), proxy port, Host %s:8080 -> %s", tc.name, ppMesh, declared)
			require.Equal(t, 200, declared.status, "a declared port's request is answered by the rule's backend, cold")

			r.reg.armed.Store(true)
			require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
			r.publish()
			time.Sleep(3 * time.Second)
			before = r.hits.Load()
			warm := r3Get(addr, host, "/")
			t.Logf("MEASURED warm, proxy port, Host %s -> %s; backend hits %d", host, warm, r.hits.Load()-before)
			require.Equal(t, 421, warm.status)
			require.Equal(t, proxy.RefusalPortNotDeclared, warm.reason)

			require.Equalf(t, tc.cold, cold.status, "cold: %s", cold)
			if tc.cold == 421 {
				require.Equal(t, warm.reason, cold.reason, "refused as warm")
				require.Zero(t, coldHits, "a refused request reached a backend")
			}
		})
	}
}
