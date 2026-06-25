package gatewayapi

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/referencegrant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// writeGatewayClassStatus sets Accepted=True and the supportedFeatures list on the
// GatewayClass whose controllerName is the edge controller and whose name is the
// one we serve. It is a no-op for any other class (we MUST only touch classes we
// control). supportedFeatures is what the upstream conformance suite reads to
// decide which test suites to run vs skip.
func (r *Reconciler) writeGatewayClassStatus(ctx context.Context) {
	gc := &gatewayv1.GatewayClass{}
	// Uncached read: GatewayClass is cluster-scoped and a cached Get can race the
	// informer sync and return NotFound on the first reconcile.
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gc); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.WarnContext(ctx, "failed to get GatewayClass for status", "gatewayClass", r.GatewayClassName, "error", err.Error())
		}
		return
	}
	if gc.Spec.ControllerName != gatewaystatus.EdgeControllerName {
		return
	}
	conds, condChanged := gatewaystatus.MergeConditions(gc.Status.Conditions, gc.Generation, gatewaystatus.Condition{
		Type:    string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:  metav1.ConditionTrue,
		Reason:  string(gatewayv1.GatewayClassReasonAccepted),
		Message: "GatewayClass accepted by the aether edge controller",
	})

	features := aetherSupportedFeatures()
	featChanged := !supportedFeaturesEqual(gc.Status.SupportedFeatures, features)

	if !condChanged && !featChanged {
		return
	}
	gc.Status.Conditions = conds
	gc.Status.SupportedFeatures = features
	if err := r.Status().Update(ctx, gc); err != nil {
		r.Log.WarnContext(ctx, "failed to write GatewayClass status", "gatewayClass", gc.Name, "error", err.Error())
	}
}

// writeGatewayStatuses sets, for each of our Gateways, top-level Accepted=True +
// Programmed=True, a per-listener status (supportedKinds, attachedRoutes,
// Programmed/Accepted/ResolvedRefs=True), and status.addresses (proposal 021
// Phase 1: the shared edge LB IP). attachedRoutes is computed from the routes the
// reconciler already listed.
func (r *Reconciler) writeGatewayStatuses(
	ctx context.Context,
	ourGateways []gatewayv1.Gateway,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
) {
	// Proposal 021 Phase 1: resolve the shared edge LoadBalancer address once and
	// publish it on every class-aether Gateway. Resolved at runtime (robust to
	// MetalLB assignment): if the LB hasn't been assigned an address yet, addrs is
	// empty and we leave status.addresses untouched this reconcile — the Service
	// watch re-triggers us once MetalLB assigns it.
	addrs := r.resolveGatewayAddresses(ctx)
	for i := range ourGateways {
		gw := &ourGateways[i]
		desired := *gw.DeepCopy()

		conds, topChanged := gatewaystatus.MergeConditions(desired.Status.Conditions, gw.Generation,
			gatewaystatus.Condition{
				Type:    string(gatewayv1.GatewayConditionAccepted),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.GatewayReasonAccepted),
				Message: "Gateway accepted by the aether edge controller",
			},
			gatewaystatus.Condition{
				Type:    string(gatewayv1.GatewayConditionProgrammed),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.GatewayReasonProgrammed),
				Message: "Gateway programmed into the aether edge data plane",
			},
		)
		desired.Status.Conditions = conds

		listeners := make([]gatewayv1.ListenerStatus, 0, len(gw.Spec.Listeners))
		for _, ln := range gw.Spec.Listeners {
			attached := r.attachedRoutesForListener(gw.Namespace, gw.Name, ln, httpRoutes, tcpRoutes, tlsRoutes, gateways, listenerKeys)
			lc, _ := gatewaystatus.MergeConditions(existingListenerConditions(gw.Status.Listeners, ln.Name), gw.Generation,
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionAccepted),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonAccepted),
					Message: "Listener accepted",
				},
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionProgrammed),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonProgrammed),
					Message: "Listener programmed",
				},
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
					Message: "All listener references resolved",
				},
			)
			listeners = append(listeners, gatewayv1.ListenerStatus{
				Name:           ln.Name,
				SupportedKinds: supportedKindsFor(ln.Protocol),
				AttachedRoutes: attached,
				Conditions:     lc,
			})
		}

		listenersChanged := !listenerStatusesEqual(gw.Status.Listeners, listeners)
		desired.Status.Listeners = listeners

		// Only publish addresses once the LB IP is resolved; never overwrite a
		// previously-published address with an empty list (which would regress the
		// Gateway to "no address"). The address write participates in the same
		// no-op-skip change detection as the conditions so we don't hot-loop.
		addrsChanged := false
		if len(addrs) > 0 {
			addrsChanged = !gatewayAddressesEqual(gw.Status.Addresses, addrs)
			desired.Status.Addresses = addrs
		}

		if !topChanged && !listenersChanged && !addrsChanged {
			continue
		}
		if err := r.Status().Update(ctx, &desired); err != nil {
			r.Log.WarnContext(ctx, "failed to write Gateway status", "gateway", gw.Name, "error", err.Error())
		}
	}
}

// resolveGatewayAddresses returns the edge's shared LoadBalancer address as a
// single Gateway status address (proposal 021 Phase 1). It Gets the edge's own
// LoadBalancer Service (EdgeServiceName in Namespace) and reads
// status.loadBalancer.ingress[0].ip, falling back to .hostname when the ingress
// is hostname-based. It returns nil (skip publication) when EdgeServiceName is
// unset, the Service is missing, or no LB address has been assigned yet — the
// Service watch re-triggers reconciliation once MetalLB assigns one.
func (r *Reconciler) resolveGatewayAddresses(ctx context.Context) []gatewayv1.GatewayStatusAddress {
	if r.EdgeServiceName == "" {
		return nil
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.EdgeServiceName}, svc); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.WarnContext(ctx, "failed to get edge Service for Gateway addresses",
				"service", r.EdgeServiceName, "namespace", r.Namespace, "error", err.Error())
		}
		return nil
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return []gatewayv1.GatewayStatusAddress{{
				Type:  ptr(gatewayv1.IPAddressType),
				Value: ing.IP,
			}}
		}
		if ing.Hostname != "" {
			return []gatewayv1.GatewayStatusAddress{{
				Type:  ptr(gatewayv1.HostnameAddressType),
				Value: ing.Hostname,
			}}
		}
	}
	return nil
}

// ptr returns a pointer to v (for optional Gateway API fields).
func ptr[T any](v T) *T { return &v }

// gatewayAddressesEqual reports whether two Gateway status address slices are
// equivalent (same type+value in order) — the no-op-skip guard for the address
// write.
func gatewayAddressesEqual(a, b []gatewayv1.GatewayStatusAddress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		at, bt := "", ""
		if a[i].Type != nil {
			at = string(*a[i].Type)
		}
		if b[i].Type != nil {
			bt = string(*b[i].Type)
		}
		if at != bt || a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

// attachedRoutesForListener counts the routes (with Accepted=True) attached to a
// specific listener. HTTP/HTTPS listeners accept HTTPRoutes attached to the
// gateway; TCP/TLS listeners accept TCP/TLSRoutes whose parentRef resolves to
// that listener's port (via gatewayParentPorts).
func (r *Reconciler) attachedRoutesForListener(
	gwNamespace, gwName string,
	ln gatewayv1.Listener,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
) int32 {
	var count int32
	switch ln.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		for i := range httpRoutes {
			hr := &httpRoutes[i]
			if httpRouteAttachesToListener(hr.Spec.ParentRefs, hr.Namespace, gwNamespace, gwName, ln) {
				count++
			}
		}
	case gatewayv1.TCPProtocolType:
		for i := range tcpRoutes {
			tr := &tcpRoutes[i]
			ports := gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, listenerKeys)
			if containsPort(ports, uint32(ln.Port)) {
				count++
			}
		}
	case gatewayv1.TLSProtocolType:
		for i := range tlsRoutes {
			tr := &tlsRoutes[i]
			ports := gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, listenerKeys)
			if containsPort(ports, uint32(ln.Port)) {
				count++
			}
		}
	}
	return count
}

// httpRouteAttachesToListener reports whether an HTTPRoute (in routeNamespace)
// attaches to the given gateway listener. A parentRef with no sectionName/port
// attaches to every HTTP/HTTPS listener; a sectionName or port narrows it to that
// listener. The parentRef namespace defaults to the route's namespace.
func httpRouteAttachesToListener(parentRefs []gatewayv1.ParentReference, routeNamespace, gwNamespace, gwName string, ln gatewayv1.Listener) bool {
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		if string(p.Name) != gwName || parentRefNamespace(p.Namespace, routeNamespace) != gwNamespace {
			continue
		}
		if p.SectionName != nil && *p.SectionName != ln.Name {
			continue
		}
		if p.Port != nil && uint32(*p.Port) != uint32(ln.Port) {
			continue
		}
		return true
	}
	return false
}

func containsPort(ports []uint32, port uint32) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// supportedKindsFor maps a listener protocol to the Route kinds the edge serves
// on it: HTTPRoute for HTTP/HTTPS, TCPRoute for TCP, TLSRoute for TLS.
func supportedKindsFor(protocol gatewayv1.ProtocolType) []gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)
	switch protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		return []gatewayv1.RouteGroupKind{{Group: &group, Kind: "HTTPRoute"}}
	case gatewayv1.TCPProtocolType:
		return []gatewayv1.RouteGroupKind{{Group: &group, Kind: "TCPRoute"}}
	case gatewayv1.TLSProtocolType:
		return []gatewayv1.RouteGroupKind{{Group: &group, Kind: "TLSRoute"}}
	default:
		return nil
	}
}

func existingListenerConditions(listeners []gatewayv1.ListenerStatus, name gatewayv1.SectionName) []metav1.Condition {
	for i := range listeners {
		if listeners[i].Name == name {
			return listeners[i].Conditions
		}
	}
	return nil
}

func listenerStatusesEqual(a, b []gatewayv1.ListenerStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].AttachedRoutes != b[i].AttachedRoutes {
			return false
		}
		if len(a[i].SupportedKinds) != len(b[i].SupportedKinds) {
			return false
		}
		for j := range a[i].SupportedKinds {
			if a[i].SupportedKinds[j].Kind != b[i].SupportedKinds[j].Kind {
				return false
			}
		}
		if len(a[i].Conditions) != len(b[i].Conditions) {
			return false
		}
		for j := range a[i].Conditions {
			ca, cb := a[i].Conditions[j], b[i].Conditions[j]
			if ca.Type != cb.Type || ca.Status != cb.Status || ca.Reason != cb.Reason ||
				ca.Message != cb.Message || ca.ObservedGeneration != cb.ObservedGeneration {
				return false
			}
		}
	}
	return true
}

// writeRouteStatuses writes our (edge controller) RouteParentStatus on every
// HTTPRoute/TCPRoute/TLSRoute attached to one of our Gateways: Accepted (True
// when attached to a matching listener, else False/NoMatchingParent) and
// ResolvedRefs (per backend resolution).
func (r *Reconciler) writeRouteStatuses(
	ctx context.Context,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
	grants []gatewayv1beta1.ReferenceGrant,
) {
	for i := range httpRoutes {
		hr := &httpRoutes[i]
		accepted := attachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, gateways)
		resolved, reason, msg := r.backendsResolveHTTP(ctx, hr.Namespace, hr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, ourGatewayParentRefs(hr.Spec.ParentRefs, hr.Namespace, gateways), accepted, resolved, reason, msg, "HTTPRoute")
	}
	for i := range tcpRoutes {
		tr := &tcpRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, listenerKeys)) > 0
		resolved, reason, msg := r.backendsResolveL4(ctx, tr.Namespace, "TCPRoute", l4BackendObjectRefs(tr.Spec.Rules), grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, resolved, reason, msg, "TCPRoute")
	}
	for i := range tlsRoutes {
		tr := &tlsRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, listenerKeys)) > 0
		resolved, reason, msg := r.backendsResolveTLS(ctx, tr.Namespace, tr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, resolved, reason, msg, "TLSRoute")
	}
}

// ourGatewayParentRefs returns the parentRefs of a route (in routeNamespace) that
// point at one of our Gateways — the only entries we own status for. The parentRef
// namespace defaults to the route's namespace.
func ourGatewayParentRefs(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[gatewayKey]struct{}) []gatewayv1.ParentReference {
	var out []gatewayv1.ParentReference
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		key := gatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; !ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// writeRouteParentStatus upserts our edge-controller RouteParentStatus for each
// Gateway parentRef, preserving other controllers' entries and only updating when
// something changed.
func (r *Reconciler) writeRouteParentStatus(
	ctx context.Context,
	obj client.Object,
	generation int64,
	status *gatewayv1.RouteStatus,
	parentRefs []gatewayv1.ParentReference,
	accepted, backendsResolved bool,
	resolvedReason, resolvedMsg, kind string,
) {
	if len(parentRefs) == 0 {
		return
	}
	parents := status.Parents
	changed := false
	acceptedStatus := metav1.ConditionTrue
	acceptedReason := string(gatewayv1.RouteReasonAccepted)
	acceptedMsg := "Route accepted by the aether edge controller"
	if !accepted {
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = string(gatewayv1.RouteReasonNoMatchingParent)
		acceptedMsg = "Route does not attach to a matching listener on this Gateway"
	}
	resolvedStatus := metav1.ConditionTrue
	if !backendsResolved {
		resolvedStatus = metav1.ConditionFalse
	}
	for _, p := range parentRefs {
		var upd bool
		parents, upd = gatewaystatus.MergeRouteParentStatus(parents, gatewaystatus.EdgeControllerName, p, generation,
			gatewaystatus.Condition{
				Type:    string(gatewayv1.RouteConditionAccepted),
				Status:  acceptedStatus,
				Reason:  acceptedReason,
				Message: acceptedMsg,
			},
			gatewaystatus.Condition{
				Type:    string(gatewayv1.RouteConditionResolvedRefs),
				Status:  resolvedStatus,
				Reason:  resolvedReason,
				Message: resolvedMsg,
			},
		)
		changed = changed || upd
	}
	if !changed {
		return
	}
	status.Parents = parents
	if err := r.Status().Update(ctx, obj); err != nil {
		r.Log.WarnContext(ctx, "failed to write route status", "kind", kind, "route", obj.GetName(), "namespace", obj.GetNamespace(), "error", err.Error())
	}
}

func (r *Reconciler) backendsResolveHTTP(ctx context.Context, ns string, rules []gatewayv1.HTTPRouteRule, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return r.backendsResolveL4(ctx, ns, "HTTPRoute", refs, grants)
}

func (r *Reconciler) backendsResolveTLS(ctx context.Context, ns string, rules []gatewayv1.TLSRouteRule, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return r.backendsResolveL4(ctx, ns, "TLSRoute", refs, grants)
}

func l4BackendObjectRefs(rules []gatewayv1alpha2.TCPRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

// backendsResolveL4 reports whether every backendRef is resolvable. aether resolves
// backends by NAME via the registry (namespace-free): the generated mesh Service
// objects live in the workload namespace, not the route's, and aren't in the edge's
// namespace-scoped cache, so a k8s Service Get is the wrong (false-negative) check.
// A valid core Service-kind ref with a non-empty name is therefore resolved here; a
// genuinely-absent backend surfaces at runtime as no endpoints / 503, not a static
// ResolvedRefs failure. Only the ref *shape* is validated.
func (r *Reconciler) backendsResolveL4(_ context.Context, routeNamespace, routeKind string, refs []gatewayv1.BackendObjectReference, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
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
