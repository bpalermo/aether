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
	commonlog "github.com/bpalermo/aether/common/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// L4RouteSink receives the projected L4 service routes and UDP routes from this
// reconciler. Typically implemented by the snapshot cache.
type L4RouteSink interface {
	// SetTCPServiceRoutes replaces the TCPRoute-derived per-service L4 rules.
	// Each key is the bare service name (parentRef.Name); values are the ordered
	// rules from all TCPRoutes attached to that service.
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
}

// SetupWithManager registers the reconciler to watch TCPRoutes, TLSRoutes, and
// UDPRoutes. Any change re-lists all three types (level-based), so a single
// fixed request enqueued for any type is sufficient.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "l4route")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1alpha2.TCPRoute{}).
		Watches(&gatewayv1.TLSRoute{}, enqueueAll).
		Watches(&gatewayv1alpha2.UDPRoute{}, enqueueAll).
		Named("l4route").
		Complete(r)
}

// Reconcile re-lists every TCPRoute, TLSRoute, and UDPRoute, keeps those
// attached to a Service (parentRef kind=Service, empty group), and replaces
// the sink's per-service rule sets.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	tcpList := &gatewayv1alpha2.TCPRouteList{}
	if err := r.List(ctx, tcpList); err != nil {
		return reconcile.Result{}, err
	}
	tlsList := &gatewayv1.TLSRouteList{}
	if err := r.List(ctx, tlsList); err != nil {
		return reconcile.Result{}, err
	}
	udpList := &gatewayv1alpha2.UDPRouteList{}
	if err := r.List(ctx, udpList); err != nil {
		return reconcile.Result{}, err
	}

	tcpRoutes := map[string][]proxy.L4ServiceRoute{}
	for i := range tcpList.Items {
		tr := &tcpList.Items[i]
		for _, svc := range serviceParents(tr.Spec.ParentRefs) {
			for _, rule := range tr.Spec.Rules {
				tcpRoutes[svc] = append(tcpRoutes[svc], r.buildTCPRoute(rule))
			}
		}
	}

	tlsRoutes := map[string][]proxy.L4ServiceRoute{}
	for i := range tlsList.Items {
		tr := &tlsList.Items[i]
		for _, svc := range serviceParents(tr.Spec.ParentRefs) {
			hostnames := make([]string, 0, len(tr.Spec.Hostnames))
			for _, h := range tr.Spec.Hostnames {
				hostnames = append(hostnames, string(h))
			}
			for _, rule := range tr.Spec.Rules {
				tlsRoutes[svc] = append(tlsRoutes[svc], r.buildTLSRoute(rule, hostnames))
			}
		}
	}

	udpRoutes := map[string][]proxy.L4Backend{}
	for i := range udpList.Items {
		ur := &udpList.Items[i]
		for _, svc := range serviceParents(ur.Spec.ParentRefs) {
			for _, rule := range ur.Spec.Rules {
				udpRoutes[svc] = append(udpRoutes[svc], r.buildUDPBackends(rule)...)
			}
		}
	}

	r.Sink.SetTCPServiceRoutes(tcpRoutes)
	r.Sink.SetTLSServiceRoutes(tlsRoutes)
	r.Sink.SetUDPServiceRoutes(udpRoutes)

	r.Log.DebugContext(ctx, "projected L4 service routes",
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
		resolved, reason, msg := r.backendsResolve(ctx, tr.Namespace, tcpBackendRefs(tr.Spec.Rules))
		if err := r.writeRouteStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, tr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write TCPRoute status", "route", tr.Name, "namespace", tr.Namespace, "error", err.Error())
		}
	}
	for i := range tlsList.Items {
		tr := &tlsList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, tr.Namespace, tlsBackendRefs(tr.Spec.Rules))
		if err := r.writeRouteStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, tr.Spec.ParentRefs, resolved, reason, msg); err != nil {
			r.Log.WarnContext(ctx, "failed to write TLSRoute status", "route", tr.Name, "namespace", tr.Namespace, "error", err.Error())
		}
	}
	for i := range udpList.Items {
		ur := &udpList.Items[i]
		resolved, reason, msg := r.backendsResolve(ctx, ur.Namespace, udpBackendRefs(ur.Spec.Rules))
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

// serviceParents returns the names of the Services these parentRefs attach to
// (kind=Service, core group — empty group string or nil group).
func serviceParents(refs []gatewayv1alpha2.ParentReference) []string {
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

// buildTCPRoute translates a TCPRouteRule into an L4ServiceRoute (no SNI match).
// Backends without a valid Service name (foreign group/kind) are skipped.
func (r *Reconciler) buildTCPRoute(rule gatewayv1alpha2.TCPRouteRule) proxy.L4ServiceRoute {
	return proxy.L4ServiceRoute{
		Backends: r.buildL4Backends(rule.BackendRefs),
	}
}

// buildTLSRoute translates a TLSRouteRule + the route's hostname list into an
// L4ServiceRoute. Hostnames map to filter_chain_match.server_names on the
// capture listener.
func (r *Reconciler) buildTLSRoute(rule gatewayv1.TLSRouteRule, hostnames []string) proxy.L4ServiceRoute {
	return proxy.L4ServiceRoute{
		SNIHostnames: hostnames,
		Backends:     r.buildL4Backends(rule.BackendRefs),
	}
}

// buildUDPBackends translates a UDPRouteRule into a flat backend list.
// UDP backends resolve to "udp:<svc>.<domain>" clusters (plain EDS, no mTLS)
// rather than the TCP "tcp:<svc>.<domain>" clusters used by TCPRoute/TLSRoute.
func (r *Reconciler) buildUDPBackends(rule gatewayv1alpha2.UDPRouteRule) []proxy.L4Backend {
	return r.buildUDPL4Backends(rule.BackendRefs)
}

// buildL4Backends converts a BackendRef slice into L4Backends with resolved
// TCP cluster names. Refs with a non-core group or non-Service kind are skipped.
func (r *Reconciler) buildL4Backends(refs []gatewayv1alpha2.BackendRef) []proxy.L4Backend {
	return r.buildBackendsWithCluster(refs, func(name string) string {
		// TCP clusters share the same EDS endpoints as HTTP clusters but use
		// ALPN "aether-tcp" (see TCPClusterName). The capture TCP floor chains
		// already reference "tcp:<svc>.<domain>" clusters.
		return proxy.TCPClusterName(name, r.MeshDomain)
	})
}

// buildUDPL4Backends converts a BackendRef slice into L4Backends with resolved
// UDP cluster names ("udp:<svc>.<domain>"). These clusters are plain EDS without
// a transport socket — UDP traffic is not covered by mesh mTLS.
func (r *Reconciler) buildUDPL4Backends(refs []gatewayv1alpha2.BackendRef) []proxy.L4Backend {
	return r.buildBackendsWithCluster(refs, func(name string) string {
		return proxy.UDPClusterName(name, r.MeshDomain)
	})
}

// buildBackendsWithCluster converts a BackendRef slice into L4Backends, resolving
// the cluster name via clusterNameFn. Refs with a non-core group or non-Service
// kind are skipped.
func (r *Reconciler) buildBackendsWithCluster(refs []gatewayv1alpha2.BackendRef, clusterNameFn func(name string) string) []proxy.L4Backend {
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
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		backends = append(backends, proxy.L4Backend{
			Service: name,
			Cluster: clusterNameFn(name),
			Weight:  weight,
		})
	}
	return backends
}
