package spire

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-spire-bridge"

// attrStream labels which subscription stream an event belongs to. Bounded
// cardinality: only svid since the Broker API replaced the node-wide bundle
// stream with the agent's own Workload API bundle (proposal 036); the attribute
// is kept so the existing series and dashboard queries are unchanged.
const attrStream = attribute.Key("aether.spire.stream")

const streamSVID = "svid"

// attrIdentity labels whose credential an update carried, and attrUpdate what the
// update was relative to what the bridge already served. Both are closed sets.
const (
	attrIdentity = attribute.Key("aether.spire.identity")
	attrUpdate   = attribute.Key("aether.spire.update")
	attrBundle   = attribute.Key("aether.spire.bundle")
)

const (
	identityPod  = "pod"
	identityNode = "node"

	// updateInitial is the first credential served for a subject in this agent
	// process (every subject after an agent restart); updateRotated replaced a
	// different one already served; updateUnchanged is a redelivery of the same
	// bytes, which is what a re-subscribe after a stream failure produces.
	updateInitial   = "initial"
	updateRotated   = "rotated"
	updateUnchanged = "unchanged"

	// bundleOwn is the agent's own Workload API trust bundle; bundleFederated is
	// one pod's federated_bundles contribution to the served union.
	bundleOwn       = "own"
	bundleFederated = "federated"
)

// bridgeMetrics holds the subscription-stream instruments. All methods are
// nil-receiver-safe so the bridge runs unchanged when telemetry is disabled.
//
// A flapping SPIRE agent used to surface only as an unexplained aether-agent
// restart (a bundle stream ending was fatal) or as a pod whose SVID silently
// stopped rotating; these counters make stream churn visible now that the
// bridge re-subscribes in place.
type bridgeMetrics struct {
	failures   metric.Int64Counter
	reconnects metric.Int64Counter

	// SDS publication-ordering instruments (issue #772). Both are zero in a
	// healthy agent, which is exactly why they are seeded: see newBridgeMetrics.
	stalePushes  metric.Int64Counter
	emptyBundles metric.Int64Counter

	// SPIFFE Broker API reference-resolution instruments (proposal 036). The
	// Broker API resolves a pod reference at request time, which the selector
	// based delegated path never did, so two failure modes are new and both are
	// seeded at zero for the same reason as the pair above.
	refNotFound metric.Int64Counter
	permDenied  metric.Int64Counter

	// Healthy-path instruments. Everything above counts a failure, so a rotation
	// — the most important thing the identity path does — left no trace at all:
	// the 2026-09-19 soak had to infer it from Envoy's per-secret version gauges
	// with every proxy-roll bucket excluded by hand.
	svidUpdates   metric.Int64Counter
	bundleUpdates metric.Int64Counter
}

// newBridgeMetrics registers the subscription-stream instruments on the given meter.
func newBridgeMetrics(meter metric.Meter) (*bridgeMetrics, error) {
	m := &bridgeMetrics{}
	var err error

	if m.failures, err = meter.Int64Counter("aether.agent.spire.stream_failures",
		metric.WithDescription("SPIFFE Broker API subscription streams that ended unexpectedly or failed to (re-)subscribe, by stream kind (svid)")); err != nil {
		return nil, fmt.Errorf("stream failures: %w", err)
	}
	if m.reconnects, err = meter.Int64Counter("aether.agent.spire.stream_reconnects",
		metric.WithDescription("SPIFFE Broker API subscription streams re-established after a failure, by stream kind (svid)")); err != nil {
		return nil, fmt.Errorf("stream reconnects: %w", err)
	}
	if m.stalePushes, err = meter.Int64Counter("aether.agent.sds_push.stale_rejected",
		metric.WithDescription("SDS pushes dropped because the snapshot already holds a newer secret generation (an out-of-order publish that would have dropped a live secret)")); err != nil {
		return nil, fmt.Errorf("stale pushes: %w", err)
	}
	if m.emptyBundles, err = meter.Int64Counter("aether.agent.sds_push.empty_bundle_skipped",
		metric.WithDescription("Bundle updates that carried no trust bundles and were skipped to keep the previously served validation contexts")); err != nil {
		return nil, fmt.Errorf("empty bundles: %w", err)
	}

	if m.refNotFound, err = meter.Int64Counter("aether.agent.spire.broker.reference_not_found",
		metric.WithDescription("SPIFFE Broker API subscribe attempts whose pod reference did not resolve (NotFound/FailedPrecondition): the pod is not in the kubelet list yet, or has no registration entry yet. Retried; a sustained rate means references are not converging")); err != nil {
		return nil, fmt.Errorf("broker reference not found: %w", err)
	}
	if m.permDenied, err = meter.Int64Counter("aether.agent.spire.broker.permission_denied",
		metric.WithDescription("SPIFFE Broker API subscribe attempts the provider refused (PermissionDenied): the broker is not authorized for this reference type, or the access policy denied impersonation of the referenced pod")); err != nil {
		return nil, fmt.Errorf("broker permission denied: %w", err)
	}

	if m.svidUpdates, err = meter.Int64Counter("aether.agent.spire.svid_updates",
		metric.WithDescription("X.509-SVIDs the bridge received and served, by identity (pod: a Broker API stream response; node: the agent's own Workload API SVID) and update (initial: first for that subject in this agent process; rotated: replaced a different one; unchanged: same bytes redelivered after a re-subscribe). rotated is the rotation signal: about one per subject per SVID half-life")); err != nil {
		return nil, fmt.Errorf("svid updates: %w", err)
	}
	if m.bundleUpdates, err = meter.Int64Counter("aether.agent.spire.bundle_updates",
		metric.WithDescription("Changes to the trust-bundle inputs behind the served validation contexts, by bundle (own: the agent's Workload API bundle; federated: one pod's federated_bundles contribution) and update (initial or rotated). own/rotated is a trust-root change")); err != nil {
		return nil, fmt.Errorf("bundle updates: %w", err)
	}

	// Seed the publication-ordering and broker-resolution counters at zero. The OTel SDK exports
	// a counter only after its first Add, so one that never increments — the
	// healthy case for both of these — never appears in Prometheus at all, and
	// "no series" is indistinguishable from "zero" to a grading query. Learned
	// the hard way on the #638 discriminator counters (issue #717).
	ctx := context.Background()
	m.stalePushes.Add(ctx, 0)
	m.emptyBundles.Add(ctx, 0)
	m.refNotFound.Add(ctx, 0)
	m.permDenied.Add(ctx, 0)
	// The healthy-path counters are seeded per attribute set for the same reason:
	// "no rotated series" must read as zero rotations, not as a missing metric.
	for _, identity := range []string{identityPod, identityNode} {
		for _, update := range []string{updateInitial, updateRotated, updateUnchanged} {
			m.svidUpdates.Add(ctx, 0, metric.WithAttributes(attrIdentity.String(identity), attrUpdate.String(update)))
		}
	}
	for _, bundle := range []string{bundleOwn, bundleFederated} {
		for _, update := range []string{updateInitial, updateRotated} {
			m.bundleUpdates.Add(ctx, 0, metric.WithAttributes(attrBundle.String(bundle), attrUpdate.String(update)))
		}
	}

	return m, nil
}

// streamFailed records one unexpected stream end or failed re-subscribe attempt.
func (m *bridgeMetrics) streamFailed(ctx context.Context, stream string) {
	if m == nil {
		return
	}
	m.failures.Add(ctx, 1, metric.WithAttributes(attrStream.String(stream)))
}

// streamReconnected records one stream re-established after a failure.
func (m *bridgeMetrics) streamReconnected(ctx context.Context, stream string) {
	if m == nil {
		return
	}
	m.reconnects.Add(ctx, 1, metric.WithAttributes(attrStream.String(stream)))
}

// stalePushRejected records one SDS push dropped for carrying an older secret
// generation than the snapshot already holds. Non-zero means publication
// ordering has been broken by a new caller and secrets could disappear from SDS.
func (m *bridgeMetrics) stalePushRejected(ctx context.Context) {
	if m == nil {
		return
	}
	m.stalePushes.Add(ctx, 1)
}

// emptyBundleSkipped records one bundle update that would have left the proxy
// with no validation contexts and was skipped in favour of the cached bundles.
func (m *bridgeMetrics) emptyBundleSkipped(ctx context.Context) {
	if m == nil {
		return
	}
	m.emptyBundles.Add(ctx, 1)
}

// referenceNotFound records one subscribe attempt whose pod reference the SPIFFE
// provider could not resolve. A handful per pod creation is the expected CNI-ADD
// race; a sustained rate means references are not converging.
func (m *bridgeMetrics) referenceNotFound(ctx context.Context) {
	if m == nil {
		return
	}
	m.refNotFound.Add(ctx, 1)
}

// permissionDenied records one subscribe attempt the SPIFFE provider refused.
// Non-zero always means a policy or configuration problem: the agent is not a
// registered broker for this reference type, or the provider's access policy
// denies it impersonating the referenced pod.
func (m *bridgeMetrics) permissionDenied(ctx context.Context) {
	if m == nil {
		return
	}
	m.permDenied.Add(ctx, 1)
}

// svidUpdated records one X.509-SVID received and served for a pod or for the
// node agent itself.
func (m *bridgeMetrics) svidUpdated(ctx context.Context, identity, update string) {
	if m == nil {
		return
	}
	m.svidUpdates.Add(ctx, 1, metric.WithAttributes(attrIdentity.String(identity), attrUpdate.String(update)))
}

// bundleUpdated records one change to a trust-bundle input.
func (m *bridgeMetrics) bundleUpdated(ctx context.Context, bundle, update string) {
	if m == nil {
		return
	}
	m.bundleUpdates.Add(ctx, 1, metric.WithAttributes(attrBundle.String(bundle), attrUpdate.String(update)))
}
