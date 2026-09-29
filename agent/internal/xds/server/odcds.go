package server

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/internal/xds/quicdemand"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/registry"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// onDemandObserver watches the discovery streams for on-demand CDS
// subscriptions (proposal 004 cold path): Envoy's CDS subscription is
// otherwise wildcard, so a *named* cluster subscription is the on_demand
// HTTP filter requesting a cluster the scoped snapshot does not carry. The
// observer records it as an observed dependency, which triggers the scoped
// reload that delivers the cluster and resumes the paused request.
type onDemandObserver struct {
	cache    *cache.SnapshotCache
	registry registry.Registry
	log      *slog.Logger
	// rejected counts on-demand requests refused because the service does
	// not exist in the catalog (nil if instrumentation disabled).
	rejected metric.Int64Counter
	// quicRefused counts on-demand `quic:` twin requests the agent refused
	// (issue #1020), by reason. Each one is a request that 503s at the
	// on_demand timeout.
	quicRefused metric.Int64Counter
	// twinRequests tells a `quic:` twin's first use apart from the proxy
	// re-stating the twins it already had on a fresh stream (issue #1033).
	twinRequests *quicdemand.Requests
}

// newOnDemandObserver creates an onDemandObserver over the snapshot cache.
// reg provides the service catalog (registry.ServiceCatalog capability) used
// to reject nonexistent services before they pollute the dependency set.
func newOnDemandObserver(snapshotCache *cache.SnapshotCache, reg registry.Registry, log *slog.Logger) *onDemandObserver {
	o := &onDemandObserver{
		cache:        snapshotCache,
		registry:     reg,
		log:          commonlog.Named(log, "odcds"),
		twinRequests: quicdemand.NewRequests(),
	}
	var err error
	if o.rejected, err = otel.Meter("aether/agent-odcds").Int64Counter("aether.agent.upstreams.rejected",
		metric.WithDescription("On-demand requests refused: the service has no endpoints anywhere in the mesh (catalog miss)")); err != nil {
		o.log.Error("failed to create rejected counter; continuing without instrumentation", "error", err)
	}
	if o.quicRefused, err = otel.Meter("aether/agent-odcds").Int64Counter("aether.agent.quic.twin.refused",
		metric.WithDescription("On-demand quic: twin requests refused (malformed name, destination not QUIC-enabled or not in the dependency set, source not a local ServiceAccount); each 503s at the on_demand timeout")); err != nil {
		o.log.Error("failed to create QUIC twin refused counter; continuing without instrumentation", "error", err)
	}
	return o
}

// Callbacks returns the go-control-plane server callbacks feeding this
// observer (the agent's proxy speaks delta ADS, so only the delta hooks are
// wired). The stream-closed hook drops that stream's on-demand subscriptions,
// which are what exempt an in-use upstream from the idle TTL.
func (o *onDemandObserver) Callbacks() serverv3.Callbacks {
	return serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: o.onDeltaRequest,
		DeltaStreamClosedFunc:  o.onDeltaStreamClosed,
	}
}

// onDeltaStreamClosed forgets every on-demand subscription the ended stream
// held. The reconnecting proxy re-subscribes from real demand; anything nobody
// asks for again ages out on the observed-dependency idle TTL. The stream's
// `quic:` twin subscriptions are released too (issue #1052): with a newer
// proxy generation live, the ended stream's process has exited, and a dormant
// pair only it held has no subscription left.
func (o *onDemandObserver) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	o.cache.CloseOnDemandStream(streamID)
	o.cache.CloseQUICStream(context.Background(), streamID)
	o.twinRequests.Close(streamID)
}

// onDeltaRequest inspects delta CDS subscriptions for on-demand cluster
// names. The wildcard subscription ("*" or empty) is the normal CDS stream;
// per-pod clusters (app_/health_) can never be on-demand requests. On-demand
// names are mesh authorities (<service>.<meshDomain>, the catch-all routes on
// the raw authority): the suffix is stripped to the bare service name before
// it enters the dependency set, and names not under the mesh domain — which
// the route table shouldn't produce — are dropped, never observed.
// A named subscription is also recorded as a LIVE on-demand subscription for
// the stream: Envoy holds it for the life of the stream, so it is the agent's
// evidence the upstream is still in use and exempts the observed dependency
// from the idle TTL (issue #682). An explicit unsubscribe releases it.
func (o *onDemandObserver) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	if req.GetTypeUrl() != resourcev3.ClusterType {
		return nil
	}
	o.resumeHeldClusters(streamID, req.GetInitialResourceVersions())
	for _, name := range req.GetResourceNamesUnsubscribe() {
		o.cache.UntrackOnDemandCluster(streamID, name)
	}
	twins := o.twinRequests.Classify(streamID, req)
	for _, name := range twins.FirstUse {
		o.observeQUICTwin(streamID, name)
	}
	o.restateTwins(streamID, twins)
	for _, name := range req.GetResourceNamesSubscribe() {
		if name == "*" || name == "" || proxy.IsPerPodClusterName(name) || proxy.IsQUICClusterName(name) {
			continue
		}
		o.observeSubscription(streamID, name)
	}
	return nil
}

// restateTwins handles the `quic:` twins a fresh stream's first CDS request
// re-stated (issue #1033; quicdemand documents the protocol facts).
//
//   - A twin the proxy merely HOLDS (delivered by the wildcard, e.g. built up
//     front by an older agent) is not demand and admits nothing. Unless its
//     pair is already known (persisted), it is answered absent --
//     go-control-plane puts a held cluster missing from the snapshot in
//     removed_resources -- and Envoy drops it; the next request that routes to
//     it opens an on-demand subscription, which is real first use. #1032
//     admitted these, which is how rev245 persisted SAs x destinations pairs.
//   - A twin the proxy re-SUBSCRIBES holds a live on-demand subscription that
//     a routed request opened and Envoy will never re-send: answering it absent
//     strands the pair (503 at the on_demand timeout, forever). Its pair is
//     admitted if valid, and marked fetched.
//
// The re-subscribed set is also the complete set of on-demand subscriptions
// of the proxy process on this stream (issue #1036), so it is handed to the
// cache on EVERY fresh stream, empty or not, recorded for this stream alone
// (issue #1052): a dormant pair no live stream holds is pruned. A hot-restart
// child's stream is the case where it is empty -- a new generation holds no
// ODCDS subscriptions -- and the draining parent's stream, if live, keeps
// vouching for its own until it ends (onDeltaStreamClosed).
//
// One line per fresh stream that re-states any twin.
func (o *onDemandObserver) restateTwins(streamID int64, twins quicdemand.Classification) {
	ctx := context.Background()
	if !twins.Fresh {
		// A later request that re-subscribes a twin it holds: same handling,
		// but it says nothing about the proxy's other subscriptions.
		o.cache.ResumeQUICSubscriptions(ctx, streamID, twins.Resubscribed)
		return
	}
	resumed := o.cache.RestateQUICSubscriptions(ctx, streamID, twins.Resubscribed)
	if len(twins.Resubscribed) == 0 && len(twins.HeldOnly) == 0 {
		return
	}
	servedHeld := 0
	for _, name := range twins.HeldOnly {
		if o.cache.HasQUICPair(name) {
			servedHeld++
		}
	}
	o.log.Info("fresh xDS stream re-stated QUIC twins: held-only twins admit nothing, live on-demand subscriptions are served",
		"stream", streamID,
		"resubscribed", len(twins.Resubscribed), "resumed_pairs", resumed,
		"held_only", len(twins.HeldOnly), "held_served", servedHeld, "answered_absent", len(twins.HeldOnly)-servedHeld)
}

// observeQUICTwin handles an on-demand request for a `quic:` twin (issue
// #1020): a local ServiceAccount's first request to a QUIC-enabled
// destination, routed by its selection arm to a twin the snapshot does not
// carry yet. Only names quicdemand classifies as first use reach it; a fresh
// stream's re-stated twins go to restateTwins (issue #1033). The cache validates
// the name and, when it admits it, publishes the twin with its load
// assignment, which answers this subscription. A
// refused name is counted: the proxy's paused request 503s (NC) at the
// on_demand timeout, so a non-zero rate here is client-visible.
//
// Deliberately NOT tracked as a live on-demand subscription: a pair has no
// idle TTL for a subscription to exempt it from (the pair is pruned only when
// its source leaves the node or its destination leaves the allow-list).
func (o *onDemandObserver) observeQUICTwin(streamID int64, name string) {
	ctx := context.Background()
	decision, reason := o.cache.ObserveQUICTwin(ctx, streamID, name)
	if decision == cache.QUICTwinRefused && o.quicRefused != nil {
		o.quicRefused.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// resumeHeldClusters re-seeds the node dependency set from the clusters the
// proxy reports it already HOLDS — the initial_resource_versions map, which the
// delta protocol requires on the first request of every stream and which is
// therefore empty on every subsequent one.
//
// This is what makes a fresh agent answer the proxy's live demand in its FIRST
// push instead of ~15s later (issue #682; see SnapshotCache.RestoreDependency
// for the full mechanism). The short version: a restarted agent starts with an
// empty observed-dependency set and drops every ODCDS-acquired upstream from
// its first snapshot, and a reconnecting Envoy will not re-ask for them — a name
// it is still "waiting for server" on is in neither initial_resource_versions
// nor resource_names_subscribe, and its on_demand filter dedupes every later
// re-subscribe. On talos (rev194, 2026-09-05) that cost 14.05s of 503s on w01
// and 14.67s on w03, ending only when Envoy's init-fetch timeout reset the
// subscription state. The held inventory is the proxy telling the agent, in the
// protocol, which clusters it is still running on; the agent simply has to read
// it.
//
// Restored entries are ordinary TTL'd observations, never live-subscription
// pins, so an upstream the proxy has stopped using still ages out of the demand
// set. A held name whose service is gone from the catalog is skipped silently —
// unlike an on-demand REQUEST, a stale held resource is not a client asking for
// a ghost, so it is neither logged nor counted as a rejection.
func (o *onDemandObserver) resumeHeldClusters(streamID int64, held map[string]string) {
	if len(held) == 0 {
		return
	}
	names := make([]string, 0, len(held))
	for name := range held {
		names = append(names, name)
	}
	// Deterministic order so the restore is reproducible across runs (map order
	// is protocol-visible on the push that follows, see #135).
	sort.Strings(names)

	ctx := context.Background()
	restored := 0
	for _, name := range names {
		if o.restoreHeldCluster(ctx, name) {
			restored++
		}
	}
	if restored > 0 {
		o.log.InfoContext(ctx, "restored node dependency set from the clusters the proxy still holds (fresh delta stream)",
			"stream", streamID, "services", restored, "held", len(held))
	}
}

// restoreHeldCluster re-admits one held cluster's demand and reports whether it
// was new to this process: a mesh service cluster re-seeds the dependency set.
// A held `quic:` twin restores nothing here (issue #1033): the proxy holds
// whatever the previous agent generation built, which on rev245 was every
// SAs x destinations twin. restateTwins handles twins on a fresh stream.
func (o *onDemandObserver) restoreHeldCluster(ctx context.Context, name string) bool {
	service, ok := o.meshServiceKey(name)
	if !ok {
		return false
	}
	if cat, hasCatalog := o.registry.(registry.ServiceCatalog); hasCatalog && !cat.HasService(service) {
		return false
	}
	return o.cache.RestoreDependency(ctx, service)
}

// meshServiceKey maps a held cluster resource name to its dependency-set service
// key, accepting ONLY a plain mesh service cluster: "<svc>.<ns>.<meshDomain>"
// with an optional ":<port>" authority suffix. Everything else the proxy holds
// is not a demand-set entry — per-pod app_/health_ clusters, the "tcp:" floor
// clusters (whose service is pinned by captureTCPDeps anyway), the passthrough
// cluster — and a lenient match would mint bogus keys like "ns/tcp:svc".
func (o *onDemandObserver) meshServiceKey(name string) (string, bool) {
	if name == "" || name == "*" || proxy.IsPerPodClusterName(name) {
		return "", false
	}
	meshDomain := o.cache.MeshDomain()
	service, ok := proxy.ServiceFromClusterName(name, meshDomain)
	if !ok {
		return "", false
	}
	base := proxy.ServiceClusterName(service, meshDomain)
	if base == "" {
		return "", false
	}
	if name == base {
		return service, true
	}
	port, isPortSuffixed := strings.CutPrefix(name, base+":")
	if !isPortSuffixed {
		return "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", false
	}
	return service, true
}

// observeSubscription maps one named delta CDS subscription to its service key,
// gates it on the catalog and records it as both an observation (the cold-path
// dependency) and a live on-demand subscription (the in-use signal).
func (o *onDemandObserver) observeSubscription(streamID int64, name string) {
	service, ok := proxy.ServiceFromClusterName(name, o.cache.MeshDomain())
	if !ok {
		o.log.Debug("ignoring on-demand subscription outside the mesh domain", "name", name)
		return
	}
	// Existence gate: the local service catalog (full mesh index, every
	// agent) rejects nonexistent services here — no dependency-set
	// pollution, no watch-filter churn, no reload. The paused request
	// fails at the on_demand timeout; a service registered moments later
	// is admitted on the client's retry (catalog events propagate in ms).
	if cat, hasCatalog := o.registry.(registry.ServiceCatalog); hasCatalog && !cat.HasService(service) {
		o.log.Info("rejecting on-demand request for unknown service", "service", service)
		if o.rejected != nil {
			o.rejected.Add(context.Background(), 1)
		}
		return
	}
	o.cache.TrackOnDemandCluster(streamID, name, service)
	o.cache.ObserveDependency(context.Background(), service)
}

// combinedCallbacks dispatches every go-control-plane server callback to all
// members, in order. Errors short-circuit (first error wins), matching how a
// single callback would fail the stream.
type combinedCallbacks []serverv3.Callbacks

var _ serverv3.Callbacks = combinedCallbacks{}

func (c combinedCallbacks) OnFetchRequest(ctx context.Context, req *discoveryv3.DiscoveryRequest) error {
	for _, cb := range c {
		if err := cb.OnFetchRequest(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func (c combinedCallbacks) OnFetchResponse(req *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
	for _, cb := range c {
		cb.OnFetchResponse(req, resp)
	}
}

func (c combinedCallbacks) OnStreamOpen(ctx context.Context, streamID int64, typeURL string) error {
	for _, cb := range c {
		if err := cb.OnStreamOpen(ctx, streamID, typeURL); err != nil {
			return err
		}
	}
	return nil
}

func (c combinedCallbacks) OnStreamClosed(streamID int64, node *corev3.Node) {
	for _, cb := range c {
		cb.OnStreamClosed(streamID, node)
	}
}

func (c combinedCallbacks) OnStreamRequest(streamID int64, req *discoveryv3.DiscoveryRequest) error {
	for _, cb := range c {
		if err := cb.OnStreamRequest(streamID, req); err != nil {
			return err
		}
	}
	return nil
}

func (c combinedCallbacks) OnStreamResponse(ctx context.Context, streamID int64, req *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
	for _, cb := range c {
		cb.OnStreamResponse(ctx, streamID, req, resp)
	}
}

func (c combinedCallbacks) OnDeltaStreamOpen(ctx context.Context, streamID int64, typeURL string) error {
	for _, cb := range c {
		if err := cb.OnDeltaStreamOpen(ctx, streamID, typeURL); err != nil {
			return err
		}
	}
	return nil
}

func (c combinedCallbacks) OnDeltaStreamClosed(streamID int64, node *corev3.Node) {
	for _, cb := range c {
		cb.OnDeltaStreamClosed(streamID, node)
	}
}

func (c combinedCallbacks) OnStreamDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	for _, cb := range c {
		if err := cb.OnStreamDeltaRequest(streamID, req); err != nil {
			return err
		}
	}
	return nil
}

func (c combinedCallbacks) OnStreamDeltaResponse(streamID int64, req *discoveryv3.DeltaDiscoveryRequest, resp *discoveryv3.DeltaDiscoveryResponse) {
	for _, cb := range c {
		cb.OnStreamDeltaResponse(streamID, req, resp)
	}
}
