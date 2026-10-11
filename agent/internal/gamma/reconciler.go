// Package gamma contains the node agent's GAMMA controller: it watches Gateway API
// HTTPRoutes and GRPCRoutes attached to a Service (parentRef kind=Service) — plus
// the ReferenceGrants that authorise cross-namespace backendRefs and the HTTPFilter
// extensions those rules attach (proposal 025) — and projects their L7 rules into
// the node proxy's routing (proposal 018, Phase 2 — east-west). The projection
// feeds BOTH route paths the proxy serves: the outbound per-service listeners and
// the transparent-capture listener's cap_http virtual hosts. Routing and mTLS are
// unchanged; the routes add canary/header/timeout vocabulary to a path the proxy
// already serves.
package gamma

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"aethermesh.dev/agent/internal/gatewaystatus"
	"aethermesh.dev/agent/internal/xds/proxy"
	configprotov1 "aethermesh.dev/api/aether/config/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/crdcheck"
	"aethermesh.dev/common/ctrlqueue"
	"aethermesh.dev/common/gammaproject"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/referencegrant"
	"aethermesh.dev/common/serviceref"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	// SetServiceInboundFilters receives the destination-side (INBOUND scope, 027 M3)
	// filters, keyed by "<ns>/<svc>": enabled on the target service's own pods'
	// inbound listeners. At most one per service.
	SetServiceInboundFilters(filters map[string]proxy.ExtensionFilter)
	// SetRouteTargetPorts receives the real Service port(s) of each route target
	// (proposal 023 M2), keyed by the same "<ns>/<svc>" route-target key as
	// SetServiceRoutes. Sourced from the HTTPRoute/GRPCRoute parentRef port; lets the
	// cap_http vhost host-match a client dialing the target's REAL port.
	SetRouteTargetPorts(ports map[string][]uint32)
	// SetServicePortMaps receives the port map of every Service a route names
	// as its parent or as a backend and that the registrar did not generate
	// (proposal 043, "A Service is a port map"), keyed "<ns>/<svc>": Service
	// port -> the pod port its targetPort names, 0 when the targetPort is a
	// name. A route's ports are ports of the SERVICE; the mesh resolves them
	// to pod ports through this.
	SetServicePortMaps(portMaps map[string]map[uint32]uint32)
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
	// grpcRouteEnabled / referenceGrantEnabled likewise gate the optional Gateway API
	// types on CRD presence (proposal 031): a partial Gateway API install degrades the
	// affected feature with a warning instead of wedging the manager.
	grpcRouteEnabled      bool
	referenceGrantEnabled bool
}

// SetupWithManager registers the reconciler to watch HTTPRoutes and GRPCRoutes. Both
// feed the same per-service rule map; any change re-lists both (level-based), so a
// single fixed request enqueued for either type is enough.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gamma")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})

	// Gateway API + HTTPFilter are EXTERNAL CRDs: watching a type whose CRD is
	// absent makes the cache fail to sync and HARD-CRASHES the manager (the
	// install-ordering crashloop). Gate every type on presence (proposal 031):
	// HTTPRoute is the floor — without it GAMMA is skipped entirely, with a
	// warning; GRPCRoute / ReferenceGrant / HTTPFilter degrade individually.
	// Detection is setup-time: installing a CRD later needs an agent restart.
	mapper := mgr.GetRESTMapper()
	httpRoutePresent, err := crdcheck.Present(mapper, gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"))
	if err != nil {
		return err
	}
	if !httpRoutePresent {
		r.Log.Warn("Gateway API CRDs not present; GAMMA east-west routing disabled until they are installed and the agent restarts")
		return nil
	}
	if r.grpcRouteEnabled, err = crdcheck.Present(mapper, gatewayv1.SchemeGroupVersion.WithKind("GRPCRoute")); err != nil {
		return err
	} else if !r.grpcRouteEnabled {
		r.Log.Warn("GRPCRoute CRD not present; gRPC method routing disabled until it is installed and the agent restarts")
	}
	// ReferenceGrant (v1beta1, the served storage version): a change to any grant
	// can flip a cross-namespace backendRef between permitted and RefNotPermitted,
	// so re-project + re-status on every grant change. Cluster-wide cache (post-#323).
	if r.referenceGrantEnabled, err = crdcheck.Present(mapper, gatewayv1beta1.SchemeGroupVersion.WithKind("ReferenceGrant")); err != nil {
		return err
	} else if !r.referenceGrantEnabled {
		r.Log.Warn("ReferenceGrant CRD not present; cross-namespace backendRefs stay RefNotPermitted until it is installed and the agent restarts")
	}
	if r.httpFilterEnabled, err = crdcheck.Present(mapper, configapisv1.GroupVersion.WithKind(configapisv1.HTTPFilterKind)); err != nil {
		return err
	} else if !r.httpFilterEnabled {
		r.Log.Warn("HTTPFilter CRD not present; proxy-extension escape hatch (025) disabled until the CRD is installed and the agent restarts")
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{})
	if r.grpcRouteEnabled {
		b = b.Watches(&gatewayv1.GRPCRoute{}, enqueueAll)
	}
	if r.referenceGrantEnabled {
		b = b.Watches(&gatewayv1beta1.ReferenceGrant{}, enqueueAll)
	}
	if r.httpFilterEnabled {
		// A change to a referenced HTTPFilter's opaque config must re-project the routes
		// that reference it.
		b = b.Watches(&configapisv1.HTTPFilter{}, enqueueAll)
	}
	// A route's ports are ports of a Service, resolved through the Service's
	// port map (proposal 043): a Service whose ports change must re-project
	// the routes that name it. Only a change of ports or of ownership counts;
	// Services change status and annotations far more often than that.
	b = b.Watches(&corev1.Service{}, enqueueAll, builder.WithPredicates(servicePortsChanged()))
	return b.Named("gamma").WithOptions(ctrlqueue.Options()).Complete(r)
}

// Reconcile re-lists every HTTPRoute and GRPCRoute, keeps those attached to a Service,
// and replaces the cache's per-service rule set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	httpList, grpcList, grants, err := r.listGammaResources(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	httpFilters, err := r.listHTTPFilters(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	specs := httpFilterSpecs(httpFilters)
	chainFilters, inboundFilters := r.buildScopedFilters(specs)

	routes, portSets := r.projectAllRoutes(httpList, grpcList, grants, httpFilters, specs)
	routeTargetPorts := buildRouteTargetPorts(portSets)
	portMaps, err := r.servicePortMaps(ctx, routes)
	if err != nil {
		return reconcile.Result{}, err
	}

	// The port maps first: the rules are placed and their backends resolved
	// through them.
	r.Sink.SetServicePortMaps(portMaps)
	r.Sink.SetServiceRoutes(routes)
	r.Sink.SetServiceChainFilters(chainFilters)
	r.Sink.SetServiceInboundFilters(inboundFilters)
	r.Sink.SetRouteTargetPorts(routeTargetPorts)
	r.Log.DebugContext(ctx, "projected gamma service routes",
		"httpRoutes", len(httpList.Items), "grpcRoutes", len(grpcList.Items), "services", len(routes))

	// Publish Gateway API status: for every Service-parented route we own, write an
	// Accepted + ResolvedRefs RouteParentStatus per Service parentRef. Status write
	// failures are logged but don't fail the reconcile (the data plane is already
	// projected); the next reconcile retries.
	r.writeHTTPRouteStatuses(ctx, httpList, grants)
	r.writeGRPCRouteStatuses(ctx, grpcList, grants)
	return reconcile.Result{}, nil
}

// listGammaResources lists HTTPRoutes, GRPCRoutes, and ReferenceGrants (all CRD-gated).
func (r *Reconciler) listGammaResources(ctx context.Context) (*gatewayv1.HTTPRouteList, *gatewayv1.GRPCRouteList, []gatewayv1beta1.ReferenceGrant, error) {
	httpList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpList); err != nil {
		return nil, nil, nil, err
	}
	// Optional types (CRD-gated in SetupWithManager, proposal 031): listing a
	// kind whose CRD is absent errors every reconcile, so skip when disabled.
	grpcList := &gatewayv1.GRPCRouteList{}
	if r.grpcRouteEnabled {
		if err := r.List(ctx, grpcList); err != nil {
			return nil, nil, nil, err
		}
	}
	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs.
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if r.referenceGrantEnabled {
		if err := r.List(ctx, grantList); err != nil {
			return nil, nil, nil, err
		}
	}
	return httpList, grpcList, grantList.Items, nil
}

// listHTTPFilters lists HTTPFilter objects indexed by "<ns>/<name>", or returns an
// empty map when the CRD is absent. HTTPFilters (proposal 025) are gated on
// httpFilterEnabled (set in SetupWithManager).
func (r *Reconciler) listHTTPFilters(ctx context.Context) (map[string]*configapisv1.HTTPFilter, error) {
	httpFilters := map[string]*configapisv1.HTTPFilter{}
	if !r.httpFilterEnabled {
		return httpFilters, nil
	}
	// HTTPFilters (config.aether.io, proposal 025) referenced by route ExtensionRefs,
	// indexed by "<ns>/<name>". ExtensionRef is same-namespace (a LocalObjectReference),
	// so no ReferenceGrant applies. Only listed when the CRD is present (gated in
	// SetupWithManager); otherwise the escape hatch is inert and the map stays empty.
	httpFilterList := &configapisv1.HTTPFilterList{}
	if err := r.List(ctx, httpFilterList); err != nil {
		return nil, err
	}
	for i := range httpFilterList.Items {
		hf := &httpFilterList.Items[i]
		httpFilters[hf.Namespace+"/"+hf.Name] = hf
	}
	return httpFilters, nil
}

// buildScopedFilters resolves per-service CHAIN and INBOUND scope extension filters
// from the HTTPFilter specs (proposal 025 M4 / 027 M3).
func (r *Reconciler) buildScopedFilters(specs map[string]*configprotov1.HTTPFilterSpec) (chainFilters, inboundFilters map[string]proxy.ExtensionFilter) {
	// Service-wide always-on filters (M4 CHAIN scope) + destination-side INBOUND
	// filters (027 M3): resolve per targeted service.
	chainFilters = map[string]proxy.ExtensionFilter{}
	inboundFilters = map[string]proxy.ExtensionFilter{}
	for key, spec := range specs {
		scope := spec.GetScope()
		if scope != configprotov1.HTTPFilterSpec_SCOPE_CHAIN && scope != configprotov1.HTTPFilterSpec_SCOPE_INBOUND {
			continue
		}
		ns, _, _ := strings.Cut(key, "/")
		r.resolveTargetRefFilters(ns, spec, specs, scope, chainFilters, inboundFilters)
	}
	return chainFilters, inboundFilters
}

// resolveTargetRefFilters resolves CHAIN/INBOUND filters for each targetRef in the spec.
func (r *Reconciler) resolveTargetRefFilters(ns string, spec *configprotov1.HTTPFilterSpec, specs map[string]*configprotov1.HTTPFilterSpec, scope configprotov1.HTTPFilterSpec_Scope, chainFilters, inboundFilters map[string]proxy.ExtensionFilter) {
	for _, t := range spec.GetTargetRefs() {
		if (t.GetGroup() != "" && t.GetGroup() != "core") || t.GetKind() != "Service" {
			continue
		}
		svcKey := ns + "/" + t.GetName()
		// Deterministic pick delegated to the shared projector (name-sorted).
		if scope == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
			if ef := gammaproject.ServiceChainFilter(svcKey, specs); ef != nil {
				chainFilters[svcKey] = proxy.ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()}
			}
		} else {
			if ef := gammaproject.ServiceInboundFilter(svcKey, specs); ef != nil {
				inboundFilters[svcKey] = proxy.ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()}
			}
		}
	}
}

// projectAllRoutes projects HTTP and GRPC routes into per-service route maps.
// It returns the route map and port sets for target-port collection.
func (r *Reconciler) projectAllRoutes(
	httpList *gatewayv1.HTTPRouteList,
	grpcList *gatewayv1.GRPCRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
) (map[string][]proxy.GammaRoute, map[string]map[uint32]struct{}) {
	routes := map[string][]proxy.GammaRoute{}
	// routeTargetPorts: the real Service port(s) each route target is addressed on
	// (proposal 023 M2), accumulated from the parentRef ports. A target may be
	// addressed on several ports across routes; collect the distinct set.
	portSets := map[string]map[uint32]struct{}{}
	r.projectHTTPRouteItems(httpList, grants, httpFilters, specs, routes, portSets)
	r.projectGRPCRouteItems(grpcList, grants, httpFilters, specs, routes, portSets)
	return routes, portSets
}

// projectHTTPRouteItems projects each HTTPRoute item into the routes and portSets maps.
func (r *Reconciler) projectHTTPRouteItems(
	httpList *gatewayv1.HTTPRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
	routes map[string][]proxy.GammaRoute,
	portSets map[string]map[uint32]struct{},
) {
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, p := range gammaproject.ServiceParents(hr.Spec.ParentRefs, hr.Namespace) {
			addToPortSets(portSets, p.Key, p.Port)
			svcFilters := gammaproject.ServiceFilters(p.Key, specs)
			for _, rule := range hr.Spec.Rules {
				gr := r.buildGammaRoute(rule, hr.Namespace, "HTTPRoute", grants, httpFilters, svcFilters)
				refs := make([]gatewayv1.BackendObjectReference, 0, len(rule.BackendRefs))
				for _, b := range rule.BackendRefs {
					refs = append(refs, b.BackendObjectReference)
				}
				routes[p.Key] = append(routes[p.Key], withPorts(gr, p.Port, gammaproject.BackendRefPorts(refs, hr.Namespace, "HTTPRoute", grants)))
			}
		}
	}
}

// projectGRPCRouteItems projects each GRPCRoute item into the routes and portSets maps.
func (r *Reconciler) projectGRPCRouteItems(
	grpcList *gatewayv1.GRPCRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
	routes map[string][]proxy.GammaRoute,
	portSets map[string]map[uint32]struct{},
) {
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, p := range gammaproject.ServiceParents(gr.Spec.ParentRefs, gr.Namespace) {
			addToPortSets(portSets, p.Key, p.Port)
			svcFilters := gammaproject.ServiceFilters(p.Key, specs)
			for _, rule := range gr.Spec.Rules {
				projected := r.buildGammaRouteFromGRPC(rule, gr.Namespace, "GRPCRoute", grants, httpFilters, svcFilters)
				refs := make([]gatewayv1.BackendObjectReference, 0, len(rule.BackendRefs))
				for _, b := range rule.BackendRefs {
					refs = append(refs, b.BackendObjectReference)
				}
				routes[p.Key] = append(routes[p.Key], withPorts(projected, p.Port, gammaproject.BackendRefPorts(refs, gr.Namespace, "GRPCRoute", grants)))
			}
		}
	}
}

// withPorts returns a projected rule carrying the two ports the shared
// projection does not: the parentRef's port (0 = none: the rule applies to
// every port of the parent Service) and each backendRef's port, both ports of
// a SERVICE (proposal 043). backendPorts is gammaproject.BackendRefPorts over
// the rule's backendRefs, so element i belongs to backend i; a length that
// does not match leaves the backends without a port rather than guess.
func withPorts(rule proxy.GammaRoute, parentPort uint32, backendPorts []uint32) proxy.GammaRoute {
	rule.ParentPort = parentPort
	if len(backendPorts) != len(rule.Backends) {
		return rule
	}
	for i := range rule.Backends {
		rule.Backends[i].Port = backendPorts[i]
	}
	return rule
}

// servicePortsChanged passes the Service events that can change a port map:
// a Service appearing or going, and an update that changes its ports or
// whether the registrar owns it.
func servicePortsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, okOld := e.ObjectOld.(*corev1.Service)
			n, okNew := e.ObjectNew.(*corev1.Service)
			if !okOld || !okNew {
				return true
			}
			return !equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports) ||
				o.Labels[aetherlabels.LabelMeshService] != n.Labels[aetherlabels.LabelMeshService]
		},
	}
}

// servicePortMaps reads the port map of every Service the projected routes
// name, as a parent or as a backend (proposal 043, "A Service is a port map").
//
// A Service the registrar generated is left out: it exposes its pods' ports
// unchanged, and the cache derives those from the registry (it does so
// whatever the Service carries, so an agent behind an older registrar needs
// nothing from it). So is a Service that does not exist: the name is then
// addressed by its pods' own ports, as before.
//
// A numeric targetPort resolves to that pod port, and an unset one to the
// Service port (the API server's own default). A NAMED targetPort cannot be
// resolved, because the registry carries no port names: it maps to 0, the
// routes through it route nothing, and it is said here.
func (r *Reconciler) servicePortMaps(ctx context.Context, routes map[string][]proxy.GammaRoute) (map[string]map[uint32]uint32, error) {
	out := map[string]map[uint32]uint32{}
	for _, key := range routeServiceKeys(routes) {
		ref, ok := serviceref.ParseKey(key)
		if !ok {
			continue
		}
		svc := &corev1.Service{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, svc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if svc.Labels[aetherlabels.LabelMeshService] == "true" {
			continue
		}
		m, named := servicePortMap(svc)
		for _, sp := range named {
			r.Log.WarnContext(ctx, "a Service port a route can address has a named targetPort, which the mesh cannot resolve (the registry carries no port names); routes through it route nothing. Use a numeric targetPort",
				"service", key, "port", sp.Port, "targetPort", sp.TargetPort.StrVal)
		}
		out[key] = m
	}
	return out, nil
}

// routeServiceKeys returns, sorted, the key of every Service the routes name:
// each parent and each backend.
func routeServiceKeys(routes map[string][]proxy.GammaRoute) []string {
	keys := map[string]struct{}{}
	for parent, rules := range routes {
		keys[parent] = struct{}{}
		for _, rule := range rules {
			for _, b := range rule.Backends {
				keys[b.Service] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// servicePortMap returns a Service's TCP port map (Service port -> pod port,
// 0 for a named targetPort) and the ports whose targetPort is a name.
func servicePortMap(svc *corev1.Service) (map[uint32]uint32, []corev1.ServicePort) {
	m := make(map[uint32]uint32, len(svc.Spec.Ports))
	var named []corev1.ServicePort
	for _, sp := range svc.Spec.Ports {
		if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
			continue
		}
		port := uint32(sp.Port)
		switch {
		case sp.TargetPort.Type == intstr.String:
			m[port] = 0
			named = append(named, sp)
		case sp.TargetPort.IntVal > 0:
			m[port] = uint32(sp.TargetPort.IntVal)
		default:
			m[port] = port
		}
	}
	return m, named
}

// addToPortSets records port in portSets[key], skipping zero ports.
func addToPortSets(portSets map[string]map[uint32]struct{}, key string, port uint32) {
	if port == 0 {
		return
	}
	if portSets[key] == nil {
		portSets[key] = map[uint32]struct{}{}
	}
	portSets[key][port] = struct{}{}
}

// buildRouteTargetPorts converts portSets (keyed by service) into sorted port slices.
func buildRouteTargetPorts(portSets map[string]map[uint32]struct{}) map[string][]uint32 {
	routeTargetPorts := make(map[string][]uint32, len(portSets))
	for key, set := range portSets {
		ports := make([]uint32, 0, len(set))
		for p := range set {
			ports = append(ports, p)
		}
		slices.Sort(ports) // stable order so the cache's change-detection is deterministic
		routeTargetPorts[key] = ports
	}
	return routeTargetPorts
}

// writeHTTPRouteStatuses publishes Gateway API status for every Service-parented HTTPRoute.
func (r *Reconciler) writeHTTPRouteStatuses(ctx context.Context, httpList *gatewayv1.HTTPRouteList, grants []gatewayv1beta1.ReferenceGrant) {
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		resolved, reason, msg := referencegrant.ResolveBackends(hr.Namespace, "HTTPRoute", httpBackendRefs(hr.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, hr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write HTTPRoute status", "route", hr.Name, "namespace", hr.Namespace, "error", err.Error())
		}
	}
}

// writeGRPCRouteStatuses publishes Gateway API status for every Service-parented GRPCRoute.
func (r *Reconciler) writeGRPCRouteStatuses(ctx context.Context, grpcList *gatewayv1.GRPCRouteList, grants []gatewayv1beta1.ReferenceGrant) {
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		resolved, reason, msg := referencegrant.ResolveBackends(gr.Namespace, "GRPCRoute", grpcBackendRefs(gr.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, gr, gr.Generation, &gr.Status.RouteStatus, gr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write GRPCRoute status", "route", gr.Name, "namespace", gr.Namespace, "error", err.Error())
		}
	}
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
