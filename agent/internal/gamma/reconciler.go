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
	"slices"
	"strings"

	"github.com/bpalermo/aether/common/serviceref"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/gammaproject"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/referencegrant"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// RouteSink receives the projected per-service GAMMA rules (the snapshot cache).
type RouteSink interface {
	SetServiceRoutes(routes map[string][]proxy.GammaRoute)
	// SetServiceChainFilters receives the service-wide ALWAYS-ON extension filters
	// (proposal 025 M4 CHAIN scope), keyed by "<ns>/<svc>": at most one per service,
	// from a CHAIN-scope HTTPFilter with a same-namespace Service targetRef. Enabled
	// at the service's capture vhost.
	SetServiceChainFilters(filters map[string]proxy.ExtensionFilter)
	// SetRouteTargetPorts receives the real Service port(s) of each route target
	// (proposal 023 M2), keyed by the same "<ns>/<svc>" route-target key as
	// SetServiceRoutes. Sourced from the HTTPRoute/GRPCRoute parentRef port; lets the
	// cap_http vhost host-match a client dialing the target's REAL port.
	SetRouteTargetPorts(ports map[string][]uint32)
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

	// httpFilterEnabled is set in SetupWithManager when the HTTPFilter CRD (proposal
	// 025) is present. When false, the reconciler neither watches nor lists HTTPFilters
	// (the escape hatch is inert) — so a missing/not-yet-installed CRD degrades
	// gracefully instead of crashing the manager on a failed informer sync.
	httpFilterEnabled bool
}

// SetupWithManager registers the reconciler to watch HTTPRoutes and GRPCRoutes. Both
// feed the same per-service rule map; any change re-lists both (level-based), so a
// single fixed request enqueued for either type is enough.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gamma")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})

	// HTTPFilter (config.aether.io, proposal 025) is an OPTIONAL CRD: gate its watch +
	// list on the CRD actually being present. Watching a type whose CRD is absent makes
	// the cache fail to sync and HARD-CRASHES the manager (the upgrade-ordering crashloop
	// when crds is installed after/with the agent). Degrade instead: skip the escape
	// hatch until the CRD exists and the agent restarts.
	gvk := configapisv1.GroupVersion.WithKind(configapisv1.HTTPFilterKind)
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			r.Log.Warn("HTTPFilter CRD not present; proxy-extension escape hatch (025) disabled until the CRD is installed and the agent restarts")
		} else {
			return fmt.Errorf("checking HTTPFilter CRD availability: %w", err)
		}
	} else {
		r.httpFilterEnabled = true
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.GRPCRoute{}, enqueueAll).
		// ReferenceGrant (v1beta1, the served storage version): a change to any grant
		// can flip a cross-namespace backendRef between permitted and RefNotPermitted,
		// so re-project + re-status on every grant change. Cluster-wide cache (post-#323).
		Watches(&gatewayv1beta1.ReferenceGrant{}, enqueueAll)
	if r.httpFilterEnabled {
		// A change to a referenced HTTPFilter's opaque config must re-project the routes
		// that reference it.
		b = b.Watches(&configapisv1.HTTPFilter{}, enqueueAll)
	}
	return b.Named("gamma").Complete(r)
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
	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs.
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := r.List(ctx, grantList); err != nil {
		return reconcile.Result{}, err
	}
	grants := grantList.Items

	// HTTPFilters (config.aether.io, proposal 025) referenced by route ExtensionRefs,
	// indexed by "<ns>/<name>". ExtensionRef is same-namespace (a LocalObjectReference),
	// so no ReferenceGrant applies. Only listed when the CRD is present (gated in
	// SetupWithManager); otherwise the escape hatch is inert and the map stays empty.
	httpFilters := map[string]*configapisv1.HTTPFilter{}
	if r.httpFilterEnabled {
		httpFilterList := &configapisv1.HTTPFilterList{}
		if err := r.List(ctx, httpFilterList); err != nil {
			return reconcile.Result{}, err
		}
		for i := range httpFilterList.Items {
			hf := &httpFilterList.Items[i]
			httpFilters[hf.Namespace+"/"+hf.Name] = hf
		}
	}

	routes := map[string][]proxy.GammaRoute{}
	// routeTargetPorts: the real Service port(s) each route target is addressed on
	// (proposal 023 M2), accumulated from the parentRef ports. A target may be
	// addressed on several ports across routes; collect the distinct set.
	portSets := map[string]map[uint32]struct{}{}
	collectPort := func(key string, port uint32) {
		if port == 0 {
			return
		}
		if portSets[key] == nil {
			portSets[key] = map[uint32]struct{}{}
		}
		portSets[key][port] = struct{}{}
	}
	// serviceFilters caches the M3 targetRef-attached HTTPFilters per route-target Service
	// (they apply to every route of that service).
	specs := httpFilterSpecs(httpFilters)
	serviceFiltersFor := func(key string) []*registryv1.ExtensionFilter {
		return gammaproject.ServiceFilters(key, specs)
	}
	// Service-wide always-on filters (M4 CHAIN scope): resolve per targeted service.
	chainFilters := map[string]proxy.ExtensionFilter{}
	for key, spec := range specs {
		if spec.GetScope() != configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
			continue
		}
		ns, _, _ := strings.Cut(key, "/")
		for _, t := range spec.GetTargetRefs() {
			if (t.GetGroup() != "" && t.GetGroup() != "core") || t.GetKind() != "Service" {
				continue
			}
			svcKey := ns + "/" + t.GetName()
			// Deterministic pick delegated to the shared projector (name-sorted).
			if ef := gammaproject.ServiceChainFilter(svcKey, specs); ef != nil {
				chainFilters[svcKey] = proxy.ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()}
			}
		}
	}
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, p := range gammaproject.ServiceParents(hr.Spec.ParentRefs, hr.Namespace) {
			collectPort(p.Key, p.Port)
			svcFilters := serviceFiltersFor(p.Key)
			for _, rule := range hr.Spec.Rules {
				routes[p.Key] = append(routes[p.Key], r.buildGammaRoute(rule, hr.Namespace, "HTTPRoute", grants, httpFilters, svcFilters))
			}
		}
	}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, p := range gammaproject.ServiceParents(gr.Spec.ParentRefs, gr.Namespace) {
			collectPort(p.Key, p.Port)
			svcFilters := serviceFiltersFor(p.Key)
			for _, rule := range gr.Spec.Rules {
				routes[p.Key] = append(routes[p.Key], r.buildGammaRouteFromGRPC(rule, gr.Namespace, "GRPCRoute", grants, httpFilters, svcFilters))
			}
		}
	}

	routeTargetPorts := make(map[string][]uint32, len(portSets))
	for key, set := range portSets {
		ports := make([]uint32, 0, len(set))
		for p := range set {
			ports = append(ports, p)
		}
		slices.Sort(ports) // stable order so the cache's change-detection is deterministic
		routeTargetPorts[key] = ports
	}

	r.Sink.SetServiceRoutes(routes)
	r.Sink.SetServiceChainFilters(chainFilters)
	r.Sink.SetRouteTargetPorts(routeTargetPorts)
	r.Log.DebugContext(ctx, "projected gamma service routes",
		"httpRoutes", len(httpList.Items), "grpcRoutes", len(grpcList.Items), "services", len(routes))

	// Publish Gateway API status: for every Service-parented route we own, write an
	// Accepted + ResolvedRefs RouteParentStatus per Service parentRef. Status write
	// failures are logged but don't fail the reconcile (the data plane is already
	// projected); the next reconcile retries.
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, hr.Namespace, "HTTPRoute", httpBackendRefs(hr.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, hr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write HTTPRoute status", "route", hr.Name, "namespace", hr.Namespace, "error", err.Error())
		}
	}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, gr.Namespace, "GRPCRoute", grpcBackendRefs(gr.Spec.Rules), grants)
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
// the ref *shape* — and, for cross-namespace refs, ReferenceGrant permission — is
// validated. routeKind is the referring route's kind (HTTPRoute/GRPCRoute), used to
// match a grant's spec.from.kind.
func (r *Reconciler) backendsResolve(_ context.Context, routeNamespace, routeKind string, refs []gatewayv1.BackendObjectReference, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	for _, ref := range refs {
		if (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			return false, string(gatewayv1.RouteReasonInvalidKind), fmt.Sprintf("backendRef %q is not a core Service", ref.Name)
		}
		if string(ref.Name) == "" {
			return false, string(gatewayv1.RouteReasonBackendNotFound), "backendRef has an empty name"
		}
		if ns := derefBackendNamespace(ref.Namespace); referencegrant.CrossNamespace(ns, routeNamespace) &&
			!referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, string(ref.Name)) {
			return false, string(gatewayv1.RouteReasonRefNotPermitted),
				fmt.Sprintf("cross-namespace backendRef to Service %q in namespace %q is not permitted by any ReferenceGrant", ref.Name, ns)
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"
}

// backendServiceKey resolves a backendRef to its namespace-qualified "<ns>/<svc>"
// registry key (020 Part 1): the backendRef's own namespace when set, else the
// route's namespace. The resulting key feeds both the data-plane cluster name
// (ServiceClusterName) and the node dependency set (routeBackendsLocked).
func backendServiceKey(backendNamespace *gatewayv1.Namespace, routeNamespace, name string) string {
	ns := routeNamespace
	if bn := derefBackendNamespace(backendNamespace); bn != "" {
		ns = bn
	}
	return serviceref.New(ns, name).Key()
}

// derefBackendNamespace returns the backendRef namespace ("" when unset).
func derefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// backendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant.
// Non-permitted cross-namespace backends are dropped from the built route (the rest of
// the route still applies), mirroring the RefNotPermitted status.
func backendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := derefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
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

// buildGammaRoute projects an HTTPRoute rule via the shared gammaproject projector and
// converts it to the agent's in-memory GammaRoute. The projector is shared with the
// registrar's cross-cluster export controller (proposal 026 EM1c) so a cluster's local
// routing and the config it exports for peers never drift.
func (r *Reconciler) buildGammaRoute(rule gatewayv1.HTTPRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter, serviceFilters []*registryv1.ExtensionFilter) proxy.GammaRoute {
	return proxy.GammaRouteFromProto(gammaproject.ProjectHTTPRule(rule, routeNamespace, routeKind, r.MeshDomain, grants, httpFilterSpecs(httpFilters), serviceFilters))
}

// buildGammaRouteFromGRPC projects a GRPCRoute rule via the shared projector.
func (r *Reconciler) buildGammaRouteFromGRPC(rule gatewayv1.GRPCRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter, serviceFilters []*registryv1.ExtensionFilter) proxy.GammaRoute {
	return proxy.GammaRouteFromProto(gammaproject.ProjectGRPCRule(rule, routeNamespace, routeKind, r.MeshDomain, grants, httpFilterSpecs(httpFilters), serviceFilters))
}

// httpFilterSpecs extracts each K8s HTTPFilter's proto spec for the shared projector
// (which is K8s-type-agnostic). Returns nil when there are none.
func httpFilterSpecs(in map[string]*configapisv1.HTTPFilter) map[string]*configprotov1.HTTPFilterSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*configprotov1.HTTPFilterSpec, len(in))
	for k, hf := range in {
		out[k] = hf.Spec
	}
	return out
}
