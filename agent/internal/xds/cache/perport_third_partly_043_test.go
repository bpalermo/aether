package cache

// Third adversarial pass on the proposal-043 stack: review test, uncommitted.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/require"
)

// Some services held, the others known from their generated Service's ports
// only: every answer must still be the one the node gives holding them all.
func TestPerPort_PartlyColdAndWarmAnswerAlike(t *testing.T) {
	seed := r3Seed(t)
	rng := rand.New(rand.NewSource(seed))
	diffs := map[string]int{}
	example := map[string]string{}
	compared := 0
	for round := 0; round < 200; round++ {
		st := r3RandomState(t, rng)
		var held []a043Service
		for _, s := range st.services {
			if rng.Intn(2) == 0 {
				held = append(held, s)
			}
		}
		part := a043Cache(t, nil)
		a043Project(part, st.services)
		part.SetServiceChainFilters(st.filters)
		part.SetServiceRoutes(st.routes)
		require.NoError(t, part.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(held)))
		warm := r3Build(t, st, true)
		pt, wt := a043Tables(t, part), a043Tables(t, warm)
		r3NoDuplicateDomains(t, fmt.Sprintf("seed %d round %d partly cold", seed, round), pt)
		legacy := map[string]uint32{}
		for _, s := range st.services {
			if s.ports[s.legacy] == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
				legacy[s.name+"."+s.ns+"."+a043Mesh] = s.legacy
			}
		}
		for tname, ptab := range pt {
			wtab := wt[tname]
			if wtab == nil {
				continue
			}
			for _, s := range st.services {
				for fi, form := range []string{s.name, s.name + "." + s.ns + "." + a043Mesh, s.name + "." + s.ns} {
					for _, suffix := range []string{"", ":80", ":8080", ":9090", ":9000", ":7777", ":18081"} {
						for _, ns := range []string{"demo", "other", "third"} {
							host := form + suffix
							ps, ws := r3Resolve(t, ptab, ns, host, legacy), r3Resolve(t, wtab, ns, host, legacy)
							compared++
							if ps.first == ws.first && fmt.Sprint(ps.all) == fmt.Sprint(ws.all) {
								continue
							}
							path := "captured"
							if tname == proxy.OutboundHTTPRouteName || tname == "cap_http_18081" {
								path = "proxy-port"
							}
							k := fmt.Sprintf("%s form=%d", path, fi)
							diffs[k]++
							if _, ok := example[k]; !ok {
								example[k] = fmt.Sprintf("round %d table %s caller %s Host %s (svc %s ports %v legacy %d, held %d of %d)\n   part: %v\n   warm: %v", round, tname, ns, host, s.key(), s.ports, s.legacy, len(held), len(st.services), ps.all, ws.all)
							}
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d answers", compared)
	for k, n := range diffs {
		t.Errorf("partly cold != warm: %s: %d times\n%s", k, n, example[k])
	}
}
