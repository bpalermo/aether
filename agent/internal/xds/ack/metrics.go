package ack

import (
	"context"
	"fmt"
	"slices"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-xds-ack"

// Attribute keys. Closed sets, every member seeded at zero (seed): the xDS
// type URLs the agent serves (ServedTypeURLs, and TypeURLOther), the wait kind
// (present/absent) and the failure reason (nack/timeout).
//
// A backend that stores OTLP attributes as Prometheus labels turns the dots
// into underscores: aether_xds_type_url, aether_xds_wait, aether_xds_reason.
const (
	attrTypeURL = attribute.Key("aether.xds.type_url")
	attrWait    = attribute.Key("aether.xds.wait")
	attrReason  = attribute.Key("aether.xds.reason")
)

// ServedTypeURLs is every xDS resource type the agent's snapshot can carry
// (the node proxy's and the edge's; agent/internal/xds/cache generateSnapshot),
// and so every type a delta response, and its NACK, can be of. It is the
// closed value set of the type-URL attribute of aether.agent.xds.nacks.
//
// A type added to the snapshot has to be added here: the cache's
// TestEveryPublishedResourceTypeHasASeededNackSeries fails until it is.
var ServedTypeURLs = []string{
	resourcev3.ListenerType,
	resourcev3.ClusterType,
	resourcev3.EndpointType,
	resourcev3.RouteType,
	resourcev3.SecretType,
	resourcev3.ExtensionConfigType,
}

// TypeURLOther is the type-URL attribute of a NACK whose response was of a
// type outside ServedTypeURLs. Nothing sends one today (the tracker only sees
// responses this agent's own server wrote); the value exists so that the label
// set stays closed whatever a response is typed as, and such a NACK is still
// counted.
const TypeURLOther = "other"

// The values of the wait and reason attributes of
// aether.agent.xds.ack_wait_failures.
const (
	waitPresent   = "present"
	waitAbsent    = "absent"
	reasonNack    = "nack"
	reasonTimeout = "timeout"
)

// trackerMetrics holds the ACK-tracking instruments. All methods are
// nil-receiver-safe so the tracker runs unchanged when telemetry is disabled.
//
// Every NACK is Envoy refusing config this agent generated (bad config, failed
// netns bind) and every wait failure is a pod lifecycle step that could not
// confirm delivery — both were previously only V(1) log lines.
type trackerMetrics struct {
	nacks        metric.Int64Counter
	waitFailures metric.Int64Counter
}

// newTrackerMetrics registers the ACK-tracking instruments on the given meter.
func newTrackerMetrics(meter metric.Meter) (*trackerMetrics, error) {
	m := &trackerMetrics{}
	var err error

	if m.nacks, err = meter.Int64Counter("aether.agent.xds.nacks",
		metric.WithDescription("Delta-xDS responses Envoy rejected (NACK with error detail), by resource type URL")); err != nil {
		return nil, fmt.Errorf("nacks: %w", err)
	}
	if m.waitFailures, err = meter.Int64Counter("aether.agent.xds.ack_wait_failures",
		metric.WithDescription("ACK waits that failed, by wait kind (present/absent) and reason (nack/timeout)")); err != nil {
		return nil, fmt.Errorf("ack wait failures: %w", err)
	}

	m.seed()
	return m, nil
}

// seed exports a zero for every series of both counters (#1480).
//
// The OTel SDK exports a counter series only after its first Add, so a counter
// that is never incremented, the healthy case for both of these, has no series
// at all, and "no NACKs" reads exactly like "this agent reports nothing". The
// runbook's published-versus-acknowledged reading of the SAN-pin gauges leans
// on the NACK counter being a readable zero. Both attribute sets are closed,
// so the series count is fixed: len(ServedTypeURLs)+1 and 4.
func (m *trackerMetrics) seed() {
	ctx := context.Background()
	for _, typeURL := range ServedTypeURLs {
		m.nacks.Add(ctx, 0, metric.WithAttributes(attrTypeURL.String(typeURL)))
	}
	m.nacks.Add(ctx, 0, metric.WithAttributes(attrTypeURL.String(TypeURLOther)))
	for _, wait := range []string{waitPresent, waitAbsent} {
		for _, reason := range []string{reasonNack, reasonTimeout} {
			m.waitFailures.Add(ctx, 0, metric.WithAttributes(attrWait.String(wait), attrReason.String(reason)))
		}
	}
}

// nacked records one rejected delta response, under its type URL when that is
// one the agent serves and under TypeURLOther when it is not, so a NACK can
// never open a series outside the seeded set.
func (m *trackerMetrics) nacked(ctx context.Context, typeURL string) {
	if m == nil {
		return
	}
	if !slices.Contains(ServedTypeURLs, typeURL) {
		typeURL = TypeURLOther
	}
	m.nacks.Add(ctx, 1, metric.WithAttributes(attrTypeURL.String(typeURL)))
}

// waitFailed records one failed ACK wait.
func (m *trackerMetrics) waitFailed(ctx context.Context, wantPresent bool, reason string) {
	if m == nil {
		return
	}
	wait := waitAbsent
	if wantPresent {
		wait = waitPresent
	}
	m.waitFailures.Add(ctx, 1, metric.WithAttributes(attrWait.String(wait), attrReason.String(reason)))
}
