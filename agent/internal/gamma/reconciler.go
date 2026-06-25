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

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	commonlog "github.com/bpalermo/aether/common/log"
	"google.golang.org/protobuf/types/known/durationpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// Publish Gateway API status: for every Service-parented route we own, write an
	// Accepted + ResolvedRefs RouteParentStatus per Service parentRef. Status write
	// failures are logged but don't fail the reconcile (the data plane is already
	// projected); the next reconcile retries.
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, hr.Namespace, httpBackendRefs(hr.Spec.Rules))
		if err := r.writeRouteStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, hr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write HTTPRoute status", "route", hr.Name, "namespace", hr.Namespace, "error", err.Error())
		}
	}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, gr.Namespace, grpcBackendRefs(gr.Spec.Rules))
		if err := r.writeRouteStatus(ctx, gr, gr.Generation, &gr.Status.RouteStatus, gr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write GRPCRoute status", "route", gr.Name, "namespace", gr.Namespace, "error", err.Error())
		}
	}
	return reconcile.Result{}, nil
}

// writeRouteStatus upserts our (mesh controller) RouteParentStatus for each
// Service parentRef of the route, setting Accepted=True (we processed it) and
// ResolvedRefs per the backend check. It preserves status entries owned by other
// controllers and only issues a status update when something changed.
func (r *Reconciler) writeRouteStatus(
	ctx context.Context,
	obj client.Object,
	generation int64,
	status *gatewayv1.RouteStatus,
	parentRefs []gatewayv1.ParentReference,
	backendsResolved bool,
	resolvedReason, resolvedMsg string,
) error {
	parents := status.Parents
	changed := false
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != "" {
			continue
		}
		if p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		conds := []gatewaystatus.Condition{{
			Type:    string(gatewayv1.RouteConditionAccepted),
			Status:  metav1.ConditionTrue,
			Reason:  string(gatewayv1.RouteReasonAccepted),
			Message: "Route accepted by the aether mesh",
		}}
		resolvedStatus := metav1.ConditionTrue
		if !backendsResolved {
			resolvedStatus = metav1.ConditionFalse
		}
		conds = append(conds, gatewaystatus.Condition{
			Type:    string(gatewayv1.RouteConditionResolvedRefs),
			Status:  resolvedStatus,
			Reason:  resolvedReason,
			Message: resolvedMsg,
		})
		var upd bool
		parents, upd = gatewaystatus.MergeRouteParentStatus(parents, gatewaystatus.MeshControllerName, p, generation, conds...)
		changed = changed || upd
	}
	if !changed {
		return nil
	}
	status.Parents = parents
	return r.Status().Update(ctx, obj)
}

// backendsResolve reports whether every backendRef is resolvable. aether resolves
// backends by NAME via the registry (namespace-free), so a valid core Service-kind
// ref with a non-empty name is resolved here; a genuinely-absent backend surfaces at
// runtime as no endpoints / 503, not a static ResolvedRefs failure. A k8s Service Get
// would be a false negative (the registry, not a k8s Service, backs the route). Only
// the ref *shape* is validated.
func (r *Reconciler) backendsResolve(_ context.Context, _ string, refs []gatewayv1.BackendObjectReference) (bool, string, string) {
	for _, ref := range refs {
		if (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			return false, string(gatewayv1.RouteReasonInvalidKind), fmt.Sprintf("backendRef %q is not a core Service", ref.Name)
		}
		if string(ref.Name) == "" {
			return false, string(gatewayv1.RouteReasonBackendNotFound), "backendRef has an empty name"
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"
}

func httpBackendRefs(rules []gatewayv1.HTTPRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

func grpcBackendRefs(rules []gatewayv1.GRPCRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
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
