package cache

// Third adversarial pass on the proposal-043 stack: review test, uncommitted.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The states where the node does NOT know the ports while cold: a generated
// Service without real ports ("names": the agent saw it before the registrar
// wrote them, or a chart 2.6 registrar wrote it) and a user Service that owns
// the name ("mapped"). Cold and warm are known to differ there; this
// classifies HOW, and fails only for the classes that matter: a cold request
// that reaches a cluster the warm node would not send it to, or reaches the
// same one with a different set of HTTP filters.
type r3Target struct {
	kind     string // "refused <reason>", "deliver", "catch-all-deliver", "fallthrough <status>", "passthrough"
	cluster  string
	filtered bool
}

func r3First(t *testing.T, table *routev3.RouteConfiguration, ns, host string) r3Target {
	t.Helper()
	return r3FirstPath(t, table, ns, host, "/")
}

func r3FirstPath(t *testing.T, table *routev3.RouteConfiguration, ns, host, path string) r3Target {
	t.Helper()
	vh := r3Pick(t, table, host)
	routes := vh.GetRoutes()
	if vh.GetMatcher() != nil {
		routes = matcherArm(t, vh.GetMatcher(), ns)
	}
	catchAll := vh.GetName() == "on_demand_catch_all"
	for _, r := range routes {
		ok := true
		for _, h := range r.GetMatch().GetHeaders() {
			if h.GetName() == ":authority" {
				ok = ok && mustRegexp(t, h.GetStringMatch().GetSafeRegex().GetRegex()).MatchString(host)
			}
		}
		if p := r.GetMatch().GetPath(); p != "" && p != path {
			ok = false
		}
		if p := r.GetMatch().GetPrefix(); p != "" && !strings.HasPrefix(path, p) {
			ok = false
		}
		if p := r.GetMatch().GetPathSeparatedPrefix(); p != "" && path != p && !strings.HasPrefix(path, p+"/") {
			ok = false
		}
		if !ok || r.GetMatch().GetRuntimeFraction() != nil {
			continue
		}
		if dr := r.GetDirectResponse(); dr != nil {
			reason := ""
			for _, h := range r.GetResponseHeadersToAdd() {
				if h.GetHeader().GetKey() == proxy.NoHTTPPortErrorHeader {
					reason = h.GetHeader().GetValue()
				}
			}
			if catchAll {
				return r3Target{kind: fmt.Sprintf("fallthrough %d", dr.GetStatus())}
			}
			return r3Target{kind: "refused " + reason}
		}
		ra := r.GetRoute()
		tg := r3Target{kind: "deliver"}
		switch {
		case ra.GetClusterHeader() != "":
			tg.cluster = host
		case ra.GetCluster() != "":
			tg.cluster = ra.GetCluster()
		case ra.GetWeightedClusters() != nil:
			tg.cluster = "weighted"
		default:
			_, tg.cluster, _ = proxy.QUICSelectionArms(r)
		}
		if tg.cluster == proxy.PassthroughClusterName {
			return r3Target{kind: "passthrough"}
		}
		_, onVh := vh.GetTypedPerFilterConfig()[r3Filter]
		_, onRoute := r.GetTypedPerFilterConfig()[r3Filter]
		tg.filtered = onVh || onRoute
		if catchAll {
			tg.kind = "catch-all-deliver"
		}
		return tg
	}
	t.Fatalf("no route for %s", host)
	return r3Target{}
}

func r3ClusterNames(t *testing.T, c *SnapshotCache) map[string]struct{} {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	out := map[string]struct{}{}
	for name := range snap.GetResources("type.googleapis.com/envoy.config.cluster.v3.Cluster") {
		out[name] = struct{}{}
	}
	return out
}

func r3BuildMode(t *testing.T, st r3State, mode string, held bool) *SnapshotCache {
	t.Helper()
	c := a043Cache(t, nil)
	switch mode {
	case "names":
		a043ProjectNamesOnly(c, st.services)
	case "mapped":
		// A user Service owns every name: a port map, no generated Service.
		// Service port = pod port - 1000 for 8080 and 9090, 80 stays 80.
		maps := map[string]map[uint32]uint32{}
		for _, s := range st.services {
			m := map[uint32]uint32{}
			for p, class := range s.ports {
				if class != registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
					continue
				}
				sp := p
				if p != 80 {
					sp = p - 1000
				}
				m[sp] = p
			}
			maps[s.key()] = m
		}
		c.SetServicePortMaps(maps)
	}
	c.SetServiceChainFilters(st.filters)
	c.SetServiceRoutes(st.routes)
	var listed []a043Service
	if held {
		listed = st.services
	}
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(listed)))
	return c
}

func TestColdModes_NameOnlyAndPortMap(t *testing.T) {
	seed := r3Seed(t)
	// Default: enough rounds to meet every class of state several times in
	// a few seconds. COLD_WARM_ROUNDS=150 is the wide run.
	rounds := 12
	if s := os.Getenv("COLD_WARM_ROUNDS"); s != "" {
		rounds, _ = strconv.Atoi(s)
	}
	for _, mode := range []string{"names", "mapped"} {
		t.Run(mode, func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			classes := map[string]int{}
			example := map[string]string{}
			bad := map[string]int{}
			limit := map[string]int{}
			for round := 0; round < rounds; round++ {
				st := r3RandomState(t, rng)
				cold, warm := r3BuildMode(t, st, mode, false), r3BuildMode(t, st, mode, true)
				ct, wt := a043Tables(t, cold), a043Tables(t, warm)
				r3NoDuplicateDomains(t, fmt.Sprintf("%s seed %d round %d cold", mode, seed, round), ct)
				r3NoDuplicateDomains(t, fmt.Sprintf("%s seed %d round %d warm", mode, seed, round), wt)
				warmClusters := r3ClusterNames(t, warm)
				legacy := map[string]uint32{}
				for _, s := range st.services {
					if s.ports[s.legacy] == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
						legacy[s.name+"."+s.ns+"."+a043Mesh] = s.legacy
					}
				}
				for tname, ctab := range ct {
					wtab := wt[tname]
					if wtab == nil {
						continue
					}
					path := "captured"
					if tname == proxy.OutboundHTTPRouteName || tname == "cap_http_18081" {
						path = "proxy-port"
					}
					for _, s := range st.services {
						forms := []string{s.name, s.name + "." + s.ns + "." + a043Mesh, s.name + "." + s.ns, s.name + "." + s.ns + ".svc.cluster.local"}
						for fi, form := range forms {
							for _, suffix := range []string{"", ":80", ":7080", ":8080", ":8090", ":9090", ":9000", ":7777", ":18081"} {
								for _, nsp := range []string{"demo /", "other /", "third /", "demo /b", "other /b"} {
									ns, reqPath, _ := strings.Cut(nsp, " ")
									host := form + suffix
									cg, wg := r3FirstPath(t, ctab, ns, host, reqPath), r3FirstPath(t, wtab, ns, host, reqPath)
									cn, wn := r3Normal(cg.cluster, legacy), r3Normal(wg.cluster, legacy)
									if cg.kind == wg.kind && cn == wn && cg.filtered == wg.filtered {
										continue
									}
									coldDelivers := cg.kind == "deliver" || cg.kind == "catch-all-deliver"
									warmDelivers := wg.kind == "deliver" || wg.kind == "catch-all-deliver"
									_, exists := warmClusters[cg.cluster]
									class := fmt.Sprintf("cold[%s] warm[%s]", cg.kind, wg.kind)
									switch {
									case coldDelivers && !exists:
										class += " cold asks for a cluster that never exists (503 at the on-demand timeout)"
									case coldDelivers && warmDelivers && cn != wn:
										class += " DIFFERENT CLUSTER, and the cold one exists"
										bad[class+" | "+path]++
									case coldDelivers && warmDelivers && cg.filtered != wg.filtered:
										class += fmt.Sprintf(" SAME CLUSTER, filter cold=%v warm=%v", cg.filtered, wg.filtered)
										bad[class+" | "+path]++
									case coldDelivers && !warmDelivers && mode == "names" && cg.kind == "deliver" && reqPath == "/b":
										// THE KNOWN LIMIT (the third review's F3, second half);
										// "/b" is the path of the states' rule with a backendRef. A
										// service known by name only (a generated Service with no
										// real port on it: a chart 2.6 registrar's) has a rule with
										// a backendRef. Its stand-in cannot tell which ports the
										// service has, so the rule's backends answer whatever port
										// the authority spells; warm, a port the service does not
										// have is refused. Nothing in the node says what the ports
										// are until it holds an endpoint. Counted, not accepted
										// silently: TestColdModes_NameOnlyAndPortMap.
										class += " KNOWN LIMIT: a name-only service's rule backends answer a port that is refused warm"
										limit[class+" | "+path]++
									case coldDelivers && !warmDelivers && mode == "mapped" && cg.kind == "catch-all-deliver":
										// THE OTHER KNOWN LIMIT. A mesh service behind a user's
										// Service that has no route and no filter: cold, the node
										// knows nothing of it (a port map alone does not make a
										// name the mesh's, the agent has one for every Service in
										// the cluster), so its full name is offered on demand by
										// the table's floor with the port read as the pods'; warm,
										// the port in an authority is the Service's, and a pod
										// port the Service has no port for is refused.
										class += " KNOWN LIMIT: an unruled, unfiltered name behind a user's Service is offered on demand by pod port"
										limit[class+" | "+path]++
									case coldDelivers && !warmDelivers:
										class += " COLD DELIVERS (the cluster exists once fetched) where warm does not"
										bad[class+" | "+path]++
									}
									key := fmt.Sprintf("%s | path=%s form=%d req=%s", class, path, fi, reqPath)
									classes[key]++
									if _, ok := example[key]; !ok {
										example[key] = fmt.Sprintf("round %d table %s caller %s Host %s svc %s ports %v legacy %d: cold %+v warm %+v", round, tname, ns, host+reqPath, s.key(), s.ports, s.legacy, cg, wg)
									}
								}
							}
						}
					}
				}
			}
			var keys []string
			for k := range classes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				t.Logf("%6d  %s\n          e.g. %s", classes[k], k, example[k])
			}
			for k, n := range bad {
				t.Errorf("%s: %d times", k, n)
			}
			for k, n := range limit {
				t.Logf("%s: %d times", k, n)
			}
			for k := range limit {
				if mode == "mapped" {
					assert.Containsf(t, k, "catch-all-deliver", "a name with a route or a filter that a user's Service owns is routed by its port map: %s", k)
				}
			}
		})
	}
}
