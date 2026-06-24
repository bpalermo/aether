// Package gamma contains the node agent's GAMMA controller: it watches Gateway API
// HTTPRoutes attached to a Service (parentRef kind=Service) and projects their L7
// rules into the node proxy's outbound routing (proposal 018, Phase 2 — east-west).
// Routing and mTLS are unchanged; HTTPRoute adds canary/header/timeout vocabulary
// to the outbound path the proxy already serves.
package gamma

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	commonlog "github.com/bpalermo/aether/common/log"
	"google.golang.org/protobuf/types/known/durationpb"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// RouteSink receives the projected per-service GAMMA rules (the snapshot cache).
type RouteSink interface {
	SetServiceRoutes(routes map[string][]proxy.GammaRoute)
}

// Reconciler watches HTTPRoutes (parentRef=Service) cluster-wide and projects, on
// any change, the complete service→rules map into the cache. Level-based: each
// reconcile re-lists, so adds/updates/deletes converge without delta tracking.
//
// Phase 2 treats every Service-parented HTTPRoute as a producer route (applies to
// all clients of that service); per-namespace consumer overrides are a follow-up.
type Reconciler struct {
	client.Client

	// Sink receives the projected per-service rules (the snapshot cache).
	Sink RouteSink
	// MeshDomain resolves a backend Service to its data-plane cluster name.
	MeshDomain string
	Log        *slog.Logger
}

// SetupWithManager registers the reconciler to watch HTTPRoutes and GRPCRoutes. Both
// feed the same per-service rule map; any change re-lists both (level-based), so a
// single fixed request enqueued for either type is enough.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gamma")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.GRPCRoute{}, enqueueAll).
		Named("gamma").
		Complete(r)
}

// Reconcile re-lists every HTTPRoute and GRPCRoute, keeps those attached to a Service,
// and replaces the cache's per-service rule set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	httpList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpList); err != nil {
		return reconcile.Result{}, err
	}
	grpcList := &gatewayv1.GRPCRouteList{}
	if err := r.List(ctx, grpcList); err != nil {
		return reconcile.Result{}, err
	}

	routes := map[string][]proxy.GammaRoute{}
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, svc := range serviceParents(hr.Spec.ParentRefs) {
			for _, rule := range hr.Spec.Rules {
				routes[svc] = append(routes[svc], r.buildGammaRoute(rule))
			}
		}
	}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, svc := range serviceParents(gr.Spec.ParentRefs) {
			for _, rule := range gr.Spec.Rules {
				routes[svc] = append(routes[svc], r.buildGammaRouteFromGRPC(rule))
			}
		}
	}

	r.Sink.SetServiceRoutes(routes)
	r.Log.DebugContext(ctx, "projected gamma service routes",
		"httpRoutes", len(httpList.Items), "grpcRoutes", len(grpcList.Items), "services", len(routes))
	return reconcile.Result{}, nil
}

// serviceParents returns the names of the Services these parentRefs attach to
// (kind=Service, core group). Shared by HTTPRoute and GRPCRoute.
func serviceParents(refs []gatewayv1.ParentReference) []string {
	var svcs []string
	for _, p := range refs {
		if p.Group != nil && string(*p.Group) != "" {
			continue
		}
		if p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		svcs = append(svcs, string(p.Name))
	}
	return svcs
}

func (r *Reconciler) buildGammaRoute(rule gatewayv1.HTTPRouteRule) proxy.GammaRoute {
	gr := proxy.GammaRoute{}
	if rule.Timeouts != nil && rule.Timeouts.Request != nil {
		// HTTPRoute timeouts are GEP-2257 duration strings (a subset of Go's).
		if d, err := time.ParseDuration(string(*rule.Timeouts.Request)); err == nil && d > 0 {
			gr.Timeout = durationpb.New(d)
		}
	}
	for _, b := range rule.BackendRefs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		gr.Backends = append(gr.Backends, proxy.GammaBackend{
			Service: name,
			Cluster: proxy.ServiceClusterName(name, r.MeshDomain),
			Weight:  weight,
		})
	}
	for _, m := range rule.Matches {
		gm := proxy.GammaMatch{}
		if m.Path != nil && m.Path.Value != nil {
			if m.Path.Type != nil && *m.Path.Type == gatewayv1.PathMatchExact {
				gm.Exact = *m.Path.Value
			} else {
				gm.Prefix = *m.Path.Value
			}
		}
		for _, h := range m.Headers {
			gm.Headers = append(gm.Headers, proxy.GammaHeaderMatch{Name: string(h.Name), Value: h.Value})
		}
		gr.Matches = append(gr.Matches, gm)
	}
	return gr
}

// buildGammaRouteFromGRPC translates a GRPCRouteRule into the same GammaRoute
// vocabulary the HTTP path uses: gRPC rides HTTP/2 as POST /<service>/<method>, so a
// method match becomes a path match (service+method = exact /svc/method; service-only
// = prefix /svc/), and gRPC header matches map to request-header matches. GRPCRoute
// has no per-rule timeout. The backends are identical (weighted Service refs).
func (r *Reconciler) buildGammaRouteFromGRPC(rule gatewayv1.GRPCRouteRule) proxy.GammaRoute {
	gr := proxy.GammaRoute{}
	for _, b := range rule.BackendRefs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		gr.Backends = append(gr.Backends, proxy.GammaBackend{
			Service: name,
			Cluster: proxy.ServiceClusterName(name, r.MeshDomain),
			Weight:  weight,
		})
	}
	for _, m := range rule.Matches {
		gm := proxy.GammaMatch{}
		if m.Method != nil {
			svc, method := "", ""
			if m.Method.Service != nil {
				svc = *m.Method.Service
			}
			if m.Method.Method != nil {
				method = *m.Method.Method
			}
			// Only the Exact method-match type maps to a literal path (the
			// GammaMatch vocabulary has no regex). svc+method = exact, svc = prefix.
			exactType := m.Method.Type == nil || *m.Method.Type == gatewayv1.GRPCMethodMatchExact
			if exactType && svc != "" && method != "" {
				gm.Exact = fmt.Sprintf("/%s/%s", svc, method)
			} else if exactType && svc != "" {
				gm.Prefix = fmt.Sprintf("/%s/", svc)
			}
		}
		for _, h := range m.Headers {
			gm.Headers = append(gm.Headers, proxy.GammaHeaderMatch{Name: string(h.Name), Value: h.Value})
		}
		gr.Matches = append(gr.Matches, gm)
	}
	return gr
}
