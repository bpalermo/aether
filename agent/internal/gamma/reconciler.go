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
	"time"

	"github.com/bpalermo/aether/common/serviceref"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/referencegrant"
	"google.golang.org/protobuf/types/known/durationpb"
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
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, p := range serviceParents(hr.Spec.ParentRefs, hr.Namespace) {
			collectPort(p.Key, p.Port)
			for _, rule := range hr.Spec.Rules {
				routes[p.Key] = append(routes[p.Key], r.buildGammaRoute(rule, hr.Namespace, "HTTPRoute", grants, httpFilters))
			}
		}
	}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, p := range serviceParents(gr.Spec.ParentRefs, gr.Namespace) {
			collectPort(p.Key, p.Port)
			for _, rule := range gr.Spec.Rules {
				routes[p.Key] = append(routes[p.Key], r.buildGammaRouteFromGRPC(rule, gr.Namespace, "GRPCRoute", grants, httpFilters))
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

// serviceParent is a resolved Service parentRef: its namespace-qualified "<ns>/<svc>"
// route-target key plus the real Service port the route attaches on (0 when the
// parentRef omits a port). Port feeds the route target's cap_http vhost domains so a
// client dialing the target's REAL port host-matches (proposal 023 M2).
type serviceParent struct {
	Key  string
	Port uint32
}

// serviceParents returns the Service parentRefs these refs attach to (kind=Service,
// core group), as namespace-qualified "<ns>/<svc>" keys plus the attached port (020
// Part 1, 023 M2). A parentRef without an explicit namespace inherits the route's
// namespace (Gateway API default). Shared by HTTPRoute and GRPCRoute.
func serviceParents(refs []gatewayv1.ParentReference, routeNamespace string) []serviceParent {
	var parents []serviceParent
	for _, p := range refs {
		if p.Group != nil && string(*p.Group) != "" {
			continue
		}
		if p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		ns := routeNamespace
		if p.Namespace != nil && string(*p.Namespace) != "" {
			ns = string(*p.Namespace)
		}
		var port uint32
		if p.Port != nil {
			port = uint32(*p.Port)
		}
		parents = append(parents, serviceParent{
			Key:  serviceref.New(ns, string(p.Name)).Key(),
			Port: port,
		})
	}
	return parents
}

func (r *Reconciler) buildGammaRoute(rule gatewayv1.HTTPRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter) proxy.GammaRoute {
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
		// Drop ungranted cross-namespace backends (RefNotPermitted): they are removed
		// from the data plane, but the rest of the route still applies.
		if !backendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		// 020 Part 1: the backend's data-plane cluster and dependency-set key are
		// namespace-qualified "<ns>/<svc>". A canary/split to a different backend
		// service therefore resolves the right registry cluster.
		key := backendServiceKey(b.Namespace, routeNamespace, name)
		gr.Backends = append(gr.Backends, proxy.GammaBackend{
			Service: key,
			Cluster: proxy.ServiceClusterName(key, r.MeshDomain),
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
	gr.HeaderMutation = buildHTTPHeaderMutation(rule.Filters)
	gr.Redirect = buildHTTPRedirect(rule.Filters)
	gr.URLRewrite = buildHTTPURLRewrite(rule.Filters)
	for _, f := range rule.Filters {
		if f.Type == gatewayv1.HTTPRouteFilterExtensionRef {
			if ef, ok := resolveExtensionFilter(f.ExtensionRef, routeNamespace, httpFilters); ok {
				gr.ExtensionFilters = append(gr.ExtensionFilters, ef)
			}
		}
	}
	return gr
}

// resolveExtensionFilter resolves a route's ExtensionRef filter (proposal 025) to an
// allow-listed proxy ExtensionFilter, or (zero, false) when it does not reference an
// in-namespace, allow-listed, ROUTE-scope HTTPFilter with a typed_config. ExtensionRef
// is a same-namespace LocalObjectReference (no namespace field), so no ReferenceGrant
// applies. CHAIN scope is deferred (admin-owned, a later milestone). Invalid/dangling
// refs are skipped here; M2 (the webhook) makes them a clean ResolvedRefs=False reject.
func resolveExtensionFilter(ref *gatewayv1.LocalObjectReference, routeNamespace string, httpFilters map[string]*configapisv1.HTTPFilter) (proxy.ExtensionFilter, bool) {
	if ref == nil ||
		string(ref.Group) != configapisv1.GroupVersion.Group ||
		string(ref.Kind) != configapisv1.HTTPFilterKind {
		return proxy.ExtensionFilter{}, false
	}
	hf, ok := httpFilters[routeNamespace+"/"+string(ref.Name)]
	if !ok || hf.Spec == nil {
		return proxy.ExtensionFilter{}, false
	}
	if hf.Spec.GetScope() == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
		return proxy.ExtensionFilter{}, false // chain scope deferred to a later milestone
	}
	name := hf.Spec.GetFilter()
	if !proxy.ExtensionFilterAllowed(name) || hf.Spec.GetTypedConfig() == nil {
		return proxy.ExtensionFilter{}, false
	}
	return proxy.ExtensionFilter{Name: name, Config: hf.Spec.GetTypedConfig()}, true
}

// buildHTTPHeaderMutation merges all RequestHeaderModifier and
// ResponseHeaderModifier filters in an HTTPRoute rule's filter list into a single
// GammaHeaderMutation. Unknown filter types (redirect, rewrite, mirror) are silently
// skipped. Returns nil when no modifier filter is present.
func buildHTTPHeaderMutation(filters []gatewayv1.HTTPRouteFilter) *proxy.GammaHeaderMutation {
	var m *proxy.GammaHeaderMutation
	ensure := func() {
		if m == nil {
			m = &proxy.GammaHeaderMutation{}
		}
	}
	for _, f := range filters {
		switch f.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier == nil {
				continue
			}
			ensure()
			for _, h := range f.RequestHeaderModifier.Set {
				m.SetRequest = append(m.SetRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			for _, h := range f.RequestHeaderModifier.Add {
				m.AddRequest = append(m.AddRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			m.RemoveRequest = append(m.RemoveRequest, f.RequestHeaderModifier.Remove...)
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier == nil {
				continue
			}
			ensure()
			for _, h := range f.ResponseHeaderModifier.Set {
				m.SetResponse = append(m.SetResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			for _, h := range f.ResponseHeaderModifier.Add {
				m.AddResponse = append(m.AddResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			m.RemoveResponse = append(m.RemoveResponse, f.ResponseHeaderModifier.Remove...)
			// Redirect, rewrite, mirror: handled by dedicated builders below; skip here.
		}
	}
	return m
}

// buildHTTPRedirect extracts the first RequestRedirect filter from an HTTPRoute
// rule's filter list and converts it to a GammaRedirect. Returns nil when no
// redirect filter is present. A rule with a RequestRedirect filter yields a redirect
// route (no backend cluster) per the Gateway API spec.
func buildHTTPRedirect(filters []gatewayv1.HTTPRouteFilter) *proxy.GammaRedirect {
	for _, f := range filters {
		if f.Type != gatewayv1.HTTPRouteFilterRequestRedirect || f.RequestRedirect == nil {
			continue
		}
		rd := f.RequestRedirect
		r := &proxy.GammaRedirect{}
		if rd.Scheme != nil {
			r.Scheme = *rd.Scheme
		}
		if rd.Hostname != nil {
			r.Hostname = string(*rd.Hostname)
		}
		if rd.Port != nil {
			r.Port = uint32(*rd.Port)
		}
		if rd.StatusCode != nil {
			r.StatusCode = *rd.StatusCode
		}
		if rd.Path != nil {
			switch rd.Path.Type {
			case gatewayv1.FullPathHTTPPathModifier:
				if rd.Path.ReplaceFullPath != nil {
					r.PathType = "ReplaceFullPath"
					r.PathValue = *rd.Path.ReplaceFullPath
				}
			case gatewayv1.PrefixMatchHTTPPathModifier:
				if rd.Path.ReplacePrefixMatch != nil {
					r.PathType = "ReplacePrefixMatch"
					r.PathValue = *rd.Path.ReplacePrefixMatch
				}
			}
		}
		return r
	}
	return nil
}

// buildHTTPURLRewrite extracts the first URLRewrite filter from an HTTPRoute
// rule's filter list and converts it to a GammaURLRewrite. Returns nil when no
// URLRewrite filter is present. URLRewrite is applied on the RouteAction (the
// request still proxies to the backend).
func buildHTTPURLRewrite(filters []gatewayv1.HTTPRouteFilter) *proxy.GammaURLRewrite {
	for _, f := range filters {
		if f.Type != gatewayv1.HTTPRouteFilterURLRewrite || f.URLRewrite == nil {
			continue
		}
		rw := f.URLRewrite
		r := &proxy.GammaURLRewrite{}
		if rw.Hostname != nil {
			r.Hostname = string(*rw.Hostname)
		}
		if rw.Path != nil {
			switch rw.Path.Type {
			case gatewayv1.FullPathHTTPPathModifier:
				if rw.Path.ReplaceFullPath != nil {
					r.PathType = "ReplaceFullPath"
					r.PathValue = *rw.Path.ReplaceFullPath
				}
			case gatewayv1.PrefixMatchHTTPPathModifier:
				if rw.Path.ReplacePrefixMatch != nil {
					r.PathType = "ReplacePrefixMatch"
					r.PathValue = *rw.Path.ReplacePrefixMatch
				}
			}
		}
		return r
	}
	return nil
}

// buildGRPCHeaderMutation merges all RequestHeaderModifier and
// ResponseHeaderModifier filters in a GRPCRoute rule's filter list into a single
// GammaHeaderMutation. The filter shape is identical to the HTTP variant
// (HTTPHeaderFilter is shared); unknown types are silently skipped.
// Returns nil when no modifier filter is present.
func buildGRPCHeaderMutation(filters []gatewayv1.GRPCRouteFilter) *proxy.GammaHeaderMutation {
	var m *proxy.GammaHeaderMutation
	ensure := func() {
		if m == nil {
			m = &proxy.GammaHeaderMutation{}
		}
	}
	for _, f := range filters {
		switch f.Type {
		case gatewayv1.GRPCRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier == nil {
				continue
			}
			ensure()
			for _, h := range f.RequestHeaderModifier.Set {
				m.SetRequest = append(m.SetRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			for _, h := range f.RequestHeaderModifier.Add {
				m.AddRequest = append(m.AddRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			m.RemoveRequest = append(m.RemoveRequest, f.RequestHeaderModifier.Remove...)
		case gatewayv1.GRPCRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier == nil {
				continue
			}
			ensure()
			for _, h := range f.ResponseHeaderModifier.Set {
				m.SetResponse = append(m.SetResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			for _, h := range f.ResponseHeaderModifier.Add {
				m.AddResponse = append(m.AddResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
			}
			m.RemoveResponse = append(m.RemoveResponse, f.ResponseHeaderModifier.Remove...)
			// Mirror and extension filter types are future items; skip silently.
		}
	}
	return m
}

// buildGammaRouteFromGRPC translates a GRPCRouteRule into the same GammaRoute
// vocabulary the HTTP path uses: gRPC rides HTTP/2 as POST /<service>/<method>, so a
// method match becomes a path match (service+method = exact /svc/method; service-only
// = prefix /svc/), and gRPC header matches map to request-header matches. GRPCRoute
// has no per-rule timeout. The backends are identical (weighted Service refs).
func (r *Reconciler) buildGammaRouteFromGRPC(rule gatewayv1.GRPCRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter) proxy.GammaRoute {
	gr := proxy.GammaRoute{}
	for _, b := range rule.BackendRefs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		if !backendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		// 020 Part 1: the backend's data-plane cluster and dependency-set key are
		// namespace-qualified "<ns>/<svc>". A canary/split to a different backend
		// service therefore resolves the right registry cluster.
		key := backendServiceKey(b.Namespace, routeNamespace, name)
		gr.Backends = append(gr.Backends, proxy.GammaBackend{
			Service: key,
			Cluster: proxy.ServiceClusterName(key, r.MeshDomain),
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
			matchType := gatewayv1.GRPCMethodMatchExact
			if m.Method.Type != nil {
				matchType = *m.Method.Type
			}
			switch matchType {
			case gatewayv1.GRPCMethodMatchExact:
				// svc+method = exact /svc/method; svc-only = prefix /svc/.
				if svc != "" && method != "" {
					gm.Exact = fmt.Sprintf("/%s/%s", svc, method)
				} else if svc != "" {
					gm.Prefix = fmt.Sprintf("/%s/", svc)
				}
			case gatewayv1.GRPCMethodMatchRegularExpression:
				// Build a /<serviceRegex>/<methodRegex> safe_regex path. When a
				// component is unset, use ".*" to match any value in that segment,
				// mirroring the Exact service-only → prefix semantics.
				svcPart := svc
				if svcPart == "" {
					svcPart = "[^/]+"
				}
				methodPart := method
				if methodPart == "" {
					methodPart = "[^/]+"
				}
				gm.Regex = fmt.Sprintf("/%s/%s", svcPart, methodPart)
			}
		}
		for _, h := range m.Headers {
			gm.Headers = append(gm.Headers, proxy.GammaHeaderMatch{Name: string(h.Name), Value: h.Value})
		}
		gr.Matches = append(gr.Matches, gm)
	}
	gr.HeaderMutation = buildGRPCHeaderMutation(rule.Filters)
	for _, f := range rule.Filters {
		if f.Type == gatewayv1.GRPCRouteFilterExtensionRef {
			if ef, ok := resolveExtensionFilter(f.ExtensionRef, routeNamespace, httpFilters); ok {
				gr.ExtensionFilters = append(gr.ExtensionFilters, ef)
			}
		}
	}
	return gr
}
