package server

import (
	"context"
	"log/slog"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.opentelemetry.io/otel"
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
}

// newOnDemandObserver creates an onDemandObserver over the snapshot cache.
// reg provides the service catalog (registry.ServiceCatalog capability) used
// to reject nonexistent services before they pollute the dependency set.
func newOnDemandObserver(snapshotCache *cache.SnapshotCache, reg registry.Registry, log *slog.Logger) *onDemandObserver {
	o := &onDemandObserver{
		cache:    snapshotCache,
		registry: reg,
		log:      commonlog.Named(log, "odcds"),
	}
	var err error
	if o.rejected, err = otel.Meter("aether/agent-odcds").Int64Counter("aether.agent.upstreams.rejected",
		metric.WithDescription("On-demand requests refused: the service has no endpoints anywhere in the mesh (catalog miss)")); err != nil {
		o.log.Error("failed to create rejected counter; continuing without instrumentation", "error", err)
	}
	return o
}

// Callbacks returns the go-control-plane server callbacks feeding this
// observer (the agent's proxy speaks delta ADS, so only the delta hook is
// wired).
func (o *onDemandObserver) Callbacks() serverv3.Callbacks {
	return serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: o.onDeltaRequest,
	}
}

// onDeltaRequest inspects delta CDS subscriptions for on-demand cluster
// names. The wildcard subscription ("*" or empty) is the normal CDS stream;
// per-pod clusters (app_/health_) can never be on-demand requests. On-demand
// names are mesh authorities (<service>.<meshDomain>, the catch-all routes on
// the raw authority): the suffix is stripped to the bare service name before
// it enters the dependency set, and names not under the mesh domain — which
// the route table shouldn't produce — are dropped, never observed.
func (o *onDemandObserver) onDeltaRequest(_ int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	if req.GetTypeUrl() != resourcev3.ClusterType {
		return nil
	}
	for _, name := range req.GetResourceNamesSubscribe() {
		if name == "*" || name == "" || proxy.IsPerPodClusterName(name) {
			continue
		}
		service, ok := proxy.ServiceFromClusterName(name, o.cache.MeshDomain())
		if !ok {
			o.log.Debug("ignoring on-demand subscription outside the mesh domain", "name", name)
			continue
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
			continue
		}
		o.cache.ObserveDependency(context.Background(), service)
	}
	return nil
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
