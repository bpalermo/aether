// Package l4route contains the node agent's L4 route controller: it watches
// Gateway API TCPRoutes, TLSRoutes, and UDPRoutes (parentRef kind=Service) and
// projects their rules into the node proxy's capture listener filter chains.
//
// TCPRoute: weighted backends replace the passthrough TCP floor chain for a service.
// TLSRoute: per-SNI filter chains (server_names match) route to weighted backends.
// UDPRoute: UDP capture listener + udp_proxy backends, with a matching CNI UDP redirect.
//
// All three route types are gated behind --l4-routes (proposal 018, Phase 3b).
// TLSRoute graduated to v1 in gateway-api v1.5.1 (and the cluster serves only v1),
// so it is watched as v1.TLSRoute; TCPRoute/UDPRoute remain v1alpha2 (not promoted).
package l4route

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/common/crdcheck"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/referencegrant"
	"github.com/bpalermo/aether/common/serviceref"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// L4RouteSink receives the projected L4 service routes and UDP routes from this
// reconciler. Typically implemented by the snapshot cache.
type L4RouteSink interface {
	// SetTCPServiceRoutes replaces the TCPRoute-derived per-service L4 rules.
	// Each key is the namespace-qualified "<ns>/<svc>" serviceref key (020 Part 1);
	// values are the ordered rules from all TCPRoutes attached to that service.
	SetTCPServiceRoutes(routes map[string][]proxy.L4ServiceRoute)
	// SetTLSServiceRoutes replaces the TLSRoute-derived per-service L4 rules.
	SetTLSServiceRoutes(routes map[string][]proxy.L4ServiceRoute)
	// SetUDPServiceRoutes replaces the UDPRoute-derived per-service UDP routes.
	SetUDPServiceRoutes(routes map[string][]proxy.L4Backend)
}

// Reconciler watches TCPRoutes, TLSRoutes, and UDPRoutes (parentRef=Service)
// cluster-wide and projects their complete per-service maps into the sink on
// any change. Level-based: each reconcile re-lists, so adds/updates/deletes
// converge without delta tracking.
type Reconciler struct {
	client.Client

	Sink       L4RouteSink
	MeshDomain string
	Log        *slog.Logger

	// Per-type CRD gates (proposal 031): TCPRoute/UDPRoute are v1alpha2
	// (Gateway API experimental channel) and TLSRoute promotion lags the core
	// types, so any of them can be absent on a standard-channel cluster.
	// Watching or listing a type whose CRD is absent wedges the manager /
	// errors every reconcile; SetupWithManager sets these from the RESTMapper
	// and the reconciler degrades per type with a warning. ReferenceGrant is
	// gated the same way.
	tcpEnabled, tlsEnabled, udpEnabled, referenceGrantEnabled bool
}

// SetupWithManager registers the reconciler to watch TCPRoutes, TLSRoutes, and
// UDPRoutes. Any change re-lists all three types (level-based), so a single
// fixed request enqueued for any type is sufficient.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "l4route")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})

	// Gate each route type on its CRD being served (proposal 031); see the
	// struct comment. All three absent = nothing to do — skip entirely.
	mapper := mgr.GetRESTMapper()
	var err error
	if r.tcpEnabled, err = crdcheck.Present(mapper, gatewayv1alpha2.SchemeGroupVersion.WithKind("TCPRoute")); err != nil {
		return err
	}
	if r.tlsEnabled, err = crdcheck.Present(mapper, gatewayv1.SchemeGroupVersion.WithKind("TLSRoute")); err != nil {
		return err
	}
	if r.udpEnabled, err = crdcheck.Present(mapper, gatewayv1alpha2.SchemeGroupVersion.WithKind("UDPRoute")); err != nil {
		return err
	}
	if !r.tcpEnabled && !r.tlsEnabled && !r.udpEnabled {
		r.Log.Warn("no L4 Gateway API CRDs (TCPRoute/TLSRoute/UDPRoute) present; L4 routing disabled until they are installed and the agent restarts")
		return nil
	}
	if !r.tcpEnabled || !r.tlsEnabled || !r.udpEnabled {
		r.Log.Warn("some L4 Gateway API CRDs absent; the corresponding route types are disabled until installed and the agent restarts",
			"tcpRoute", r.tcpEnabled, "tlsRoute", r.tlsEnabled, "udpRoute", r.udpEnabled)
	}
	if r.referenceGrantEnabled, err = crdcheck.Present(mapper, gatewayv1beta1.SchemeGroupVersion.WithKind("ReferenceGrant")); err != nil {
		return err
	} else if !r.referenceGrantEnabled {
		r.Log.Warn("ReferenceGrant CRD not present; cross-namespace L4 backendRefs stay RefNotPermitted until it is installed and the agent restarts")
	}

	// For() needs a present primary type; the rest join as Watches.
	watches := []struct {
		enabled bool
		obj     client.Object
	}{
		{r.tcpEnabled, &gatewayv1alpha2.TCPRoute{}},
		{r.tlsEnabled, &gatewayv1.TLSRoute{}},
		{r.udpEnabled, &gatewayv1alpha2.UDPRoute{}},
		{r.referenceGrantEnabled, &gatewayv1beta1.ReferenceGrant{}},
	}
	var b *builder.Builder
	for _, w := range watches {
		if !w.enabled {
			continue
		}
		if b == nil {
			b = ctrl.NewControllerManagedBy(mgr).For(w.obj)
			continue
		}
		b = b.Watches(w.obj, enqueueAll)
	}
	return b.Named("l4route").Complete(r)
}

// Reconcile re-lists every TCPRoute, TLSRoute, and UDPRoute, keeps those
// attached to a Service (parentRef kind=Service, empty group), and replaces
// the sink's per-service rule sets.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Every List is CRD-gated (proposal 031): listing a kind whose CRD is
	// absent errors every reconcile, so disabled types keep empty lists.
	tcpList := &gatewayv1alpha2.TCPRouteList{}
	if r.tcpEnabled {
		if err := r.List(ctx, tcpList); err != nil {
			return reconcile.Result{}, err
		}
	}
	tlsList := &gatewayv1.TLSRouteList{}
	if r.tlsEnabled {
		if err := r.List(ctx, tlsList); err != nil {
			return reconcile.Result{}, err
		}
	}
	udpList := &gatewayv1alpha2.UDPRouteList{}
	if r.udpEnabled {
		if err := r.List(ctx, udpList); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs.
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if r.referenceGrantEnabled {
		if err := r.List(ctx, grantList); err != nil {
			return reconcile.Result{}, err
		}
	}
	grants := grantList.Items

	tcpRoutes := map[string][]proxy.L4ServiceRoute{}
	for i := range tcpList.Items {
		tr := &tcpList.Items[i]
		for _, svc := range serviceParents(tr.Spec.ParentRefs, tr.Namespace) {
			for _, rule := range tr.Spec.Rules {
				tcpRoutes[svc] = append(tcpRoutes[svc], r.buildTCPRoute(rule, tr.Namespace, "TCPRoute", grants))
			}
		}
	}

	tlsRoutes := map[string][]proxy.L4ServiceRoute{}
	for i := range tlsList.Items {
		tr := &tlsList.Items[i]
		for _, svc := range serviceParents(tr.Spec.ParentRefs, tr.Namespace) {
			hostnames := make([]string, 0, len(tr.Spec.Hostnames))
			for _, h := range tr.Spec.Hostnames {
				hostnames = append(hostnames, string(h))
			}
			for _, rule := range tr.Spec.Rules {
				tlsRoutes[svc] = append(tlsRoutes[svc], r.buildTLSRoute(rule, hostnames, tr.Namespace, "TLSRoute", grants))
			}
		}
	}

	udpRoutes := map[string][]proxy.L4Backend{}
	for i := range udpList.Items {
		ur := &udpList.Items[i]
		for _, svc := range serviceParents(ur.Spec.ParentRefs, ur.Namespace) {
			for _, rule := range ur.Spec.Rules {
				udpRoutes[svc] = append(udpRoutes[svc], r.buildUDPBackends(rule, ur.Namespace, "UDPRoute", grants)...)
			}
		}
	}

	r.Sink.SetTCPServiceRoutes(tcpRoutes)
	r.Sink.SetTLSServiceRoutes(tlsRoutes)
	r.Sink.SetUDPServiceRoutes(udpRoutes)

	r.Log.DebugContext(
		ctx, "projected L4 service routes",
		"tcpRoutes", len(tcpList.Items),
		"tlsRoutes", len(tlsList.Items),
		"udpRoutes", len(udpList.Items),
		"tcpServices", len(tcpRoutes),
		"tlsServices", len(tlsRoutes),
		"udpServices", len(udpRoutes),
	)

	// Publish Gateway API status: Accepted + ResolvedRefs RouteParentStatus for
	// every Service parentRef of each route we own (mesh controller). Failures are
	// logged, not fatal — the data plane is already projected and the next
	// reconcile retries.
	for i := range tcpList.Items {
		tr := &tcpList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, tr.Namespace, "TCPRoute", tcpBackendRefs(tr.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, tr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write TCPRoute status", "route", tr.Name, "namespace", tr.Namespace, "error", err.Error())
		}
	}
	for i := range tlsList.Items {
		tr := &tlsList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, tr.Namespace, "TLSRoute", tlsBackendRefs(tr.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, tr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write TLSRoute status", "route", tr.Name, "namespace", tr.Namespace, "error", err.Error())
		}
	}
	for i := range udpList.Items {
		ur := &udpList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, ur.Namespace, "UDPRoute", udpBackendRefs(ur.Spec.Rules), grants)
		if err := r.writeRouteStatus(ctx, ur, ur.Generation, &ur.Status.RouteStatus, ur.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write UDPRoute status", "route", ur.Name, "namespace", ur.Namespace, "error", err.Error())
		}
	}
	return reconcile.Result{}, nil
}

// writeRouteStatus upserts our (mesh controller) RouteParentStatus for each
// Service parentRef of the route, setting Accepted=True and ResolvedRefs per the
// backend check, preserving entries owned by other controllers and only issuing
// a status update when something changed.
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
		resolvedStatus := metav1.ConditionTrue
		if !backendsResolved {
			resolvedStatus = metav1.ConditionFalse
		}
		conds := []gatewaystatus.Condition{
			{
				Type:    string(gatewayv1.RouteConditionAccepted),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.RouteReasonAccepted),
				Message: "Route accepted by the aether mesh",
			},
			{
				Type:    string(gatewayv1.RouteConditionResolvedRefs),
				Status:  resolvedStatus,
				Reason:  resolvedReason,
				Message: resolvedMsg,
			},
		}
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
// validated. routeKind is the referring route's kind (TCPRoute/TLSRoute/UDPRoute).
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

// derefBackendNamespace returns the backendRef namespace ("" when unset).
func derefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// backendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant.
// Non-permitted cross-namespace backends are dropped from the built route.
func backendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := derefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}

func tcpBackendRefs(rules []gatewayv1alpha2.TCPRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

func tlsBackendRefs(rules []gatewayv1.TLSRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

func udpBackendRefs(rules []gatewayv1alpha2.UDPRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

// serviceParents returns the namespace-qualified "<ns>/<svc>" keys of the Service
// parentRefs (kind=Service, core group — empty group string or nil group; 020
// Part 1). A parentRef without an explicit namespace inherits the route's
// namespace (Gateway API default).
func serviceParents(refs []gatewayv1alpha2.ParentReference, routeNamespace string) []string {
	var svcs []string
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
		svcs = append(svcs, serviceref.New(ns, string(p.Name)).Key())
	}
	return svcs
}

// backendServiceKey resolves a backendRef to its namespace-qualified "<ns>/<svc>"
// registry key (020 Part 1): the backendRef's own namespace when set, else the
// route's namespace. The resulting key feeds both the data-plane cluster name
// (TCP/TLS/UDP ClusterName) and the node dependency set (L4Backend.Service).
func backendServiceKey(backendNamespace *gatewayv1.Namespace, routeNamespace, name string) string {
	ns := routeNamespace
	if bn := derefBackendNamespace(backendNamespace); bn != "" {
		ns = bn
	}
	return serviceref.New(ns, name).Key()
}

// buildTCPRoute translates a TCPRouteRule into an L4ServiceRoute (no SNI match).
// Backends without a valid Service name (foreign group/kind) are skipped.
func (r *Reconciler) buildTCPRoute(rule gatewayv1alpha2.TCPRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) proxy.L4ServiceRoute {
	return proxy.L4ServiceRoute{
		Backends: r.buildL4Backends(rule.BackendRefs, routeNamespace, routeKind, grants),
	}
}

// buildTLSRoute translates a TLSRouteRule + the route's hostname list into an
// L4ServiceRoute. Hostnames map to filter_chain_match.server_names on the
// capture listener.
func (r *Reconciler) buildTLSRoute(rule gatewayv1.TLSRouteRule, hostnames []string, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) proxy.L4ServiceRoute {
	return proxy.L4ServiceRoute{
		SNIHostnames: hostnames,
		Backends:     r.buildL4Backends(rule.BackendRefs, routeNamespace, routeKind, grants),
	}
}

// buildUDPBackends translates a UDPRouteRule into a flat backend list.
// UDP backends resolve to "udp:<svc>.<domain>" clusters (plain EDS, no mTLS)
// rather than the TCP "tcp:<svc>.<domain>" clusters used by TCPRoute/TLSRoute.
func (r *Reconciler) buildUDPBackends(rule gatewayv1alpha2.UDPRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) []proxy.L4Backend {
	return r.buildUDPL4Backends(rule.BackendRefs, routeNamespace, routeKind, grants)
}

// buildL4Backends converts a BackendRef slice into L4Backends with resolved
// TCP cluster names. Refs with a non-core group or non-Service kind are skipped.
func (r *Reconciler) buildL4Backends(refs []gatewayv1alpha2.BackendRef, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) []proxy.L4Backend {
	return r.buildBackendsWithCluster(refs, routeNamespace, routeKind, grants, func(key string) string {
		// TCP clusters share the same EDS endpoints as HTTP clusters but use
		// ALPN "aether-tcp" (see TCPClusterName). The capture TCP floor chains
		// already reference "tcp:<svc>.<ns>.<domain>" clusters. The key is the
		// backend's namespace-qualified "<ns>/<svc>" (020 Part 1).
		return proxy.TCPClusterName(key, r.MeshDomain)
	})
}

// buildUDPL4Backends converts a BackendRef slice into L4Backends with resolved
// UDP cluster names ("udp:<svc>.<domain>"). These clusters are plain EDS without
// a transport socket — UDP traffic is not covered by mesh mTLS.
func (r *Reconciler) buildUDPL4Backends(refs []gatewayv1alpha2.BackendRef, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) []proxy.L4Backend {
	return r.buildBackendsWithCluster(refs, routeNamespace, routeKind, grants, func(key string) string {
		// key is the backend's namespace-qualified "<ns>/<svc>" (020 Part 1).
		return proxy.UDPClusterName(key, r.MeshDomain)
	})
}

// buildBackendsWithCluster converts a BackendRef slice into L4Backends, resolving
// the cluster name via clusterNameFn. Refs with a non-core group or non-Service
// kind are skipped, as are ungranted cross-namespace refs (RefNotPermitted: dropped
// from the data plane, mirroring the route's ResolvedRefs status).
func (r *Reconciler) buildBackendsWithCluster(refs []gatewayv1alpha2.BackendRef, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, clusterNameFn func(key string) string) []proxy.L4Backend {
	backends := make([]proxy.L4Backend, 0, len(refs))
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		if name == "" {
			continue
		}
		if !backendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		// 020 Part 1: the backend's data-plane cluster and dependency-set key are
		// namespace-qualified "<ns>/<svc>" (backendRef namespace if set, else the
		// route's). A split to a different backend service therefore resolves the
		// right registry cluster.
		key := backendServiceKey(b.Namespace, routeNamespace, name)
		backends = append(backends, proxy.L4Backend{
			Service: key,
			Cluster: clusterNameFn(key),
			Weight:  weight,
		})
	}
	return backends
}
