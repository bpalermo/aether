package cache

import (
	"context"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotHCMsCarryTheConfiguredDrainTimeout: every HTTP connection
// manager in a node snapshot's listeners carries the drain_timeout set with
// SetConnectionDrainTimeout (--proxy-connection-drain-timeout), or
// proxy.DefaultConnectionDrainTimeout when none was set. An HCM left at
// Envoy's 5s default would leave upstream's proactive drain window empty under
// the chart's 10s drain time.
func TestSnapshotHCMsCarryTheConfiguredDrainTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"default", 0, proxy.DefaultConnectionDrainTimeout},
		{"configured", 3 * time.Second, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache("node-1")
			if tc.set != 0 {
				c.SetConnectionDrainTimeout(tc.set)
			}
			ctx := context.Background()
			require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
				Name: "a-0", Namespace: "demo", ServiceAccount: "source-a",
				NetworkNamespace: "/var/run/netns/cni-a-0",
			}, "aether.internal"))
			require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)

			hcms := 0
			for name, res := range snap.GetResources(resourcev3.ListenerType) {
				l := res.(*listenerv3.Listener)
				chains := append([]*listenerv3.FilterChain{}, l.GetFilterChains()...)
				if l.GetDefaultFilterChain() != nil {
					chains = append(chains, l.GetDefaultFilterChain())
				}
				for _, fc := range chains {
					for _, f := range fc.GetFilters() {
						if f.GetName() != "envoy.http_connection_manager" {
							continue
						}
						hcm := &http_connection_managerv3.HttpConnectionManager{}
						require.NoError(t, f.GetTypedConfig().UnmarshalTo(hcm))
						hcms++
						require.NotNil(t, hcm.GetDrainTimeout(), "%s/%s: drain_timeout must be set", name, fc.GetName())
						assert.Equal(t, tc.want, hcm.GetDrainTimeout().AsDuration(), "%s/%s", name, fc.GetName())
					}
				}
			}
			// The pod's inbound, outbound and capture listeners at least.
			assert.GreaterOrEqual(t, hcms, 3, "too few HTTP connection managers in the snapshot to trust this test")
		})
	}
}
