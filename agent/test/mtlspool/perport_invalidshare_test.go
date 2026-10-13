package mtlspool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// TestPerPort_LiveInvalidBackendShareAndTheValidSplit measures what a rule
// with an invalid backendRef does to the requests that are NOT answered 500.
//
// The share of an invalid backendRef is a route with a runtime_fraction that
// answers 500, in front of the route with the valid backends' weighted
// clusters. The proxy draws ONE random value per request and uses it for both
// (config_impl.cc hands the same random_value to the route's runtime match and
// to the weighted-cluster pick): the 500 route takes the requests whose value
// modulo 1 000 000 is below the share, and the pick among the valid backends
// is the value modulo their total weight. The requests that are left all have
// a value in the upper part of the range, so with a total weight that shares
// factors with a million the pick is made from a biased set.
//
// Measured on this proxy before the weights were made independent
// (proxy.independentWeights), 1200 requests each:
//
//	valid 1 and 1, invalid 2:                    500: 592   first: 300   second: 308
//	valid 500000 and 500000, invalid 1000000:    500: 595   first:   0   second: 605
//
// The second is every delivered request on one backend. Both must be even.
func TestPerPort_LiveInvalidBackendShareAndTheValidSplit(t *testing.T) {
	const n = 1200
	for _, tc := range []struct {
		name          string
		a, b, invalid uint32
	}{
		{"valid 1 and 1, invalid 2", 1, 1, 2},
		{"valid 500000 and 500000, invalid 1000000", 500000, 500000, 1000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, capture, _ := ppStartFrom(t, "demo")
			// Which valid backend answered: a response header per weighted
			// cluster, added to the generated table. It observes the pick and
			// does not take part in it.
			r.mutate = func(res map[resourcev3.Type][]types.Resource) {
				for _, rc := range res[resourcev3.RouteType] {
					for _, vh := range rc.(*routev3.RouteConfiguration).GetVirtualHosts() {
						for _, rt := range vh.GetRoutes() {
							for _, cw := range rt.GetRoute().GetWeightedClusters().GetClusters() {
								cw.ResponseHeadersToAdd = []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x-picked", Value: cw.GetName()}}}
							}
						}
					}
				}
			}
			r.c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {{
				Matches: []proxy.GammaMatch{{Prefix: "/"}},
				Backends: []proxy.GammaBackend{
					{Service: "demo/other", Cluster: "other.demo.aether.internal", Port: 8080, Weight: tc.a},
					{Service: "elsewhere/multi", Cluster: "multi.elsewhere.aether.internal", Port: 8080, Weight: tc.b},
					{Service: "demo/other", Cluster: "other.demo.aether.internal", Port: 7777, Weight: tc.invalid},
				},
			}}})
			require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
			r.publish()
			time.Sleep(3 * time.Second)

			counts := map[string]int{}
			for range n {
				got := flGet(capture, ppMesh+":8080")
				require.NoError(t, got.err)
				key := fmt.Sprint(got.status)
				if got.status == 200 {
					key = got.header.Get("X-Picked")
				}
				counts[key]++
			}
			t.Logf("MEASURED %s, %d requests: %v", tc.name, n, counts)
			refused := counts["500"]
			first, second := counts["other.demo.aether.internal"], counts["multi.elsewhere.aether.internal"]
			require.Equal(t, n, refused+first+second, "every request is a 500 or one of the two valid backends: %v", counts)
			// The invalid share is half: 500 for about half of the requests.
			require.InDelta(t, n/2, refused, n*0.06, "the share answered 500")
			require.InDelta(t, (n-refused)/2, first, n*0.06, "the valid backends share the rest evenly")
		})
	}
}
