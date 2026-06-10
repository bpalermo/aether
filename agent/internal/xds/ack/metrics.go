package ack

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-xds-ack"

// Attribute keys. Bounded cardinality: xDS type URLs (a handful), wait kind
// (present/absent) and failure reason (nack/timeout).
const (
	attrTypeURL = attribute.Key("aether.xds.type_url")
	attrWait    = attribute.Key("aether.xds.wait")
	attrReason  = attribute.Key("aether.xds.reason")
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

	return m, nil
}

// nacked records one rejected delta response.
func (m *trackerMetrics) nacked(ctx context.Context, typeURL string) {
	if m == nil {
		return
	}
	m.nacks.Add(ctx, 1, metric.WithAttributes(attrTypeURL.String(typeURL)))
}

// waitFailed records one failed ACK wait.
func (m *trackerMetrics) waitFailed(ctx context.Context, wantPresent bool, reason string) {
	if m == nil {
		return
	}
	wait := "absent"
	if wantPresent {
		wait = "present"
	}
	m.waitFailures.Add(ctx, 1, metric.WithAttributes(attrWait.String(wait), attrReason.String(reason)))
}
