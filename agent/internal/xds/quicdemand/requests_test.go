package quicdemand

import (
	"testing"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
)

const (
	twinA = "quic:echo.demo.aether.internal@demo/source-a"
	twinB = "quic:echo.demo.aether.internal@demo/source-b"
	twinC = "quic:echo.demo.aether.internal@demo/source-c"
)

func cds(subscribe []string, held map[string]string) *discoveryv3.DeltaDiscoveryRequest {
	return &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                 resourcev3.ClusterType,
		ResourceNamesSubscribe:  subscribe,
		InitialResourceVersions: held,
	}
}

func TestClassify(t *testing.T) {
	r := NewRequests()

	// A fresh stream's first CDS request: a re-subscribed twin is a live
	// on-demand subscription (held or still waiting); a twin that is only held
	// came through the wildcard. Neither is first use.
	got := r.Classify(1, cds(
		[]string{"*", twinB, twinA, twinA, "echo.demo.aether.internal"},
		map[string]string{twinA: "v1", twinC: "v1", "app_pod_8080": "v1", "echo.demo.aether.internal": "v1"},
	))
	assert.Empty(t, got.FirstUse, "nothing in a stream's first CDS request is first use")
	assert.Equal(t, []string{twinA, twinB}, got.Resubscribed, "sorted, de-duplicated")
	assert.Equal(t, []string{twinC}, got.HeldOnly)
	assert.True(t, got.Fresh, "the stream's first CDS request re-states its subscriptions")

	// A later request naming a twin is the on_demand filter: first use.
	got = r.Classify(1, cds([]string{twinC, twinC, "other.demo.aether.internal"}, nil))
	assert.Equal(t, Classification{FirstUse: []string{twinC}}, got)

	// A later request that still carries a held twin is not first use.
	got = r.Classify(1, cds([]string{twinA}, map[string]string{twinA: "v1"}))
	assert.Equal(t, Classification{Resubscribed: []string{twinA}}, got)

	// Other type URLs neither classify nor consume a stream's first request.
	assert.Equal(t, Classification{}, r.Classify(2, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ListenerType, ResourceNamesSubscribe: []string{twinA},
	}))
	got = r.Classify(2, cds([]string{"*", twinA}, nil))
	assert.Equal(t, Classification{Resubscribed: []string{twinA}, Fresh: true}, got, "stream 2's first CDS request, after an LDS one, is still its first")

	// A closed stream's id reused (a reconnect) starts over.
	r.Close(1)
	got = r.Classify(1, cds([]string{"*"}, map[string]string{twinB: "v1"}))
	assert.Equal(t, Classification{HeldOnly: []string{twinB}, Fresh: true}, got)
}
