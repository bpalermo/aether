package ack

import (
	"context"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// counterSeries returns every data point of the named counter, keyed by its
// attributes rendered as "k=v,k=v" (in key order). ok is false when the
// counter has exported nothing at all.
func counterSeries(t *testing.T, reader *sdkmetric.ManualReader, name string) (map[string]int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %s is not an int64 sum", name)
			out := make(map[string]int64, len(sum.DataPoints))
			for _, dp := range sum.DataPoints {
				key := ""
				for _, kv := range dp.Attributes.ToSlice() {
					if key != "" {
						key += ","
					}
					key += string(kv.Key) + "=" + kv.Value.AsString()
				}
				_, dup := out[key]
				require.False(t, dup, "%s: two data points for {%s}", name, key)
				out[key] = dp.Value
			}
			return out, true
		}
	}
	return nil, false
}

// The closed set, stated here on its own so that the test fails if the
// production list loses a type the agent serves. The cache's
// TestEveryPublishedResourceTypeHasASeededNackSeries holds the list to what a
// snapshot actually publishes.
var wantServedTypeURLs = []string{
	resourcev3.ListenerType,
	resourcev3.ClusterType,
	resourcev3.EndpointType,
	resourcev3.RouteType,
	resourcev3.SecretType,
	resourcev3.ExtensionConfigType,
}

const typeURLAttr = "aether.xds.type_url="

// nack sends one delta response of the given type on stream 1 and rejects it.
func nack(t *testing.T, tr *Tracker, typeURL, nonce string) {
	t.Helper()
	tr.onDeltaResponse(1, nil, &discoveryv3.DeltaDiscoveryResponse{
		TypeUrl: typeURL, Nonce: nonce,
		Resources: []*discoveryv3.Resource{{Name: "r-" + nonce}},
	})
	require.NoError(t, tr.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: typeURL, ResponseNonce: nonce,
		ErrorDetail: status.New(codes.InvalidArgument, "rejected").Proto(),
	}))
}

// TestMetrics_NacksSeededPerTypeURL is #1480. The counter used to be created
// by its first NACK, so a proxy that never rejected anything had no series and
// "no NACKs" could not be told from "not reporting". Every resource type the
// agent serves has a series at zero from the start, a NACK increments its own
// type's and only that one, and nothing can open a series outside the set.
func TestMetrics_NacksSeededPerTypeURL(t *testing.T) {
	const name = "aether.agent.xds.nacks"
	tr, reader := newTestTracker(t)

	want := map[string]int64{typeURLAttr + "other": 0}
	for _, u := range wantServedTypeURLs {
		want[typeURLAttr+u] = 0
	}
	got, ok := counterSeries(t, reader, name)
	require.True(t, ok, "before any NACK the counter must already export: an absent series is not a zero")
	require.Equal(t, want, got, "one series per served type URL and the catch-all, each at zero")

	// A cluster update is rejected.
	nack(t, tr, resourcev3.ClusterType, "c1")
	want[typeURLAttr+resourcev3.ClusterType] = 1
	got, _ = counterSeries(t, reader, name)
	require.Equal(t, want, got, "the NACK is counted under its own type URL and nowhere else")

	// A type URL outside the set is counted, under the catch-all: the label set
	// stays closed whatever a response is typed as.
	nack(t, tr, "type.googleapis.com/envoy.service.runtime.v3.Runtime", "r1")
	want[typeURLAttr+"other"] = 1
	got, _ = counterSeries(t, reader, name)
	require.Equal(t, want, got, "an unserved type URL must not open a series of its own")
}

// TestMetrics_WaitFailuresSeeded is the other half of #1480: the four series
// of the wait-failure counter (present/absent by nack/timeout) exist at zero
// before any wait has failed.
func TestMetrics_WaitFailuresSeeded(t *testing.T) {
	const name = "aether.agent.xds.ack_wait_failures"
	tr, reader := newTestTracker(t)

	want := map[string]int64{
		"aether.xds.reason=nack,aether.xds.wait=absent":     0,
		"aether.xds.reason=nack,aether.xds.wait=present":    0,
		"aether.xds.reason=timeout,aether.xds.wait=absent":  0,
		"aether.xds.reason=timeout,aether.xds.wait=present": 0,
	}
	got, ok := counterSeries(t, reader, name)
	require.True(t, ok, "before any failed wait the counter must already export")
	require.Equal(t, want, got)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.Error(t, tr.WaitListenerPresent(ctx, testListener))
	want["aether.xds.reason=timeout,aether.xds.wait=present"] = 1
	got, _ = counterSeries(t, reader, name)
	require.Equal(t, want, got, "a failed wait increments its own series; no series is added")
}
