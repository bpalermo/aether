package gatewayapi

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// writeGatewayClassStatus sets Accepted=True on the GatewayClass whose
// controllerName is the edge controller and whose name is the one we serve. It
// is a no-op for any other class (we MUST only touch classes we control).
func (r *Reconciler) writeGatewayClassStatus(ctx context.Context) {
	gc := &gatewayv1.GatewayClass{}
	// Uncached read: GatewayClass is cluster-scoped and the manager cache is
	// namespace-scoped, so r.Get (cached) would miss and return NotFound.
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gc); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.WarnContext(ctx, "failed to get GatewayClass for status", "gatewayClass", r.GatewayClassName, "error", err.Error())
		}
		return
	}
	if gc.Spec.ControllerName != gatewaystatus.EdgeControllerName {
		return
	}
	conds, changed := gatewaystatus.MergeConditions(gc.Status.Conditions, gc.Generation, gatewaystatus.Condition{
		Type:    string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:  metav1.ConditionTrue,
		Reason:  string(gatewayv1.GatewayClassReasonAccepted),
		Message: "GatewayClass accepted by the aether edge controller",
	})
	if !changed {
		return
	}
	gc.Status.Conditions = conds
	if err := r.Status().Update(ctx, gc); err != nil {
		r.Log.WarnContext(ctx, "failed to write GatewayClass status", "gatewayClass", gc.Name, "error", err.Error())
	}
}

// writeGatewayStatuses sets, for each of our Gateways, top-level Accepted=True +
// Programmed=True and a per-listener status (supportedKinds, attachedRoutes,
// Programmed/Accepted/ResolvedRefs=True). attachedRoutes is computed from the
// routes the reconciler already listed.
func (r *Reconciler) writeGatewayStatuses(
	ctx context.Context,
	ourGateways []gatewayv1.Gateway,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[string]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
) {
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
			attached := r.attachedRoutesForListener(gw.Name, ln, httpRoutes, tcpRoutes, tlsRoutes, gateways, listenerKeys)
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
		if !topChanged && !listenersChanged {
			continue
		}
		if err := r.Status().Update(ctx, &desired); err != nil {
			r.Log.WarnContext(ctx, "failed to write Gateway status", "gateway", gw.Name, "error", err.Error())
		}
	}
}

// attachedRoutesForListener counts the routes (with Accepted=True) attached to a
// specific listener. HTTP/HTTPS listeners accept HTTPRoutes attached to the
// gateway; TCP/TLS listeners accept TCP/TLSRoutes whose parentRef resolves to
// that listener's port (via gatewayParentPorts).
func (r *Reconciler) attachedRoutesForListener(
	gwName string,
	ln gatewayv1.Listener,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[string]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
) int32 {
	var count int32
	switch ln.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		for i := range httpRoutes {
			hr := &httpRoutes[i]
			if httpRouteAttachesToListener(hr.Spec.ParentRefs, gwName, ln) {
				count++
			}
		}
	case gatewayv1.TCPProtocolType:
		for i := range tcpRoutes {
			tr := &tcpRoutes[i]
			ports := gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TCPProtocolType, listenerKeys)
			if containsPort(ports, uint32(ln.Port)) {
				count++
			}
		}
	case gatewayv1.TLSProtocolType:
		for i := range tlsRoutes {
			tr := &tlsRoutes[i]
			ports := gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TLSProtocolType, listenerKeys)
			if containsPort(ports, uint32(ln.Port)) {
				count++
			}
		}
	}
	return count
}

// httpRouteAttachesToListener reports whether an HTTPRoute's parentRefs attach to
// the given gateway listener. A parentRef with no sectionName/port attaches to
// every HTTP/HTTPS listener; a sectionName or port narrows it to that listener.
func httpRouteAttachesToListener(parentRefs []gatewayv1.ParentReference, gwName string, ln gatewayv1.Listener) bool {
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		if string(p.Name) != gwName {
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
	gateways map[string]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
) {
	for i := range httpRoutes {
		hr := &httpRoutes[i]
		accepted := attachedToOurGateway(hr.Spec.ParentRefs, gateways)
		resolved, reason, msg := r.backendsResolveHTTP(ctx, hr.Namespace, hr.Spec.Rules)
		r.writeRouteParentStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, ourGatewayParentRefs(hr.Spec.ParentRefs, gateways), accepted, resolved, reason, msg, "HTTPRoute")
	}
	for i := range tcpRoutes {
		tr := &tcpRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TCPProtocolType, listenerKeys)) > 0
		resolved, reason, msg := r.backendsResolveL4(ctx, tr.Namespace, l4BackendObjectRefs(tr.Spec.Rules))
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, gateways), accepted, resolved, reason, msg, "TCPRoute")
	}
	for i := range tlsRoutes {
		tr := &tlsRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TLSProtocolType, listenerKeys)) > 0
		resolved, reason, msg := r.backendsResolveTLS(ctx, tr.Namespace, tr.Spec.Rules)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, gateways), accepted, resolved, reason, msg, "TLSRoute")
	}
}

// ourGatewayParentRefs returns the parentRefs of a route that point at one of our
// Gateways — the only entries we own status for.
func ourGatewayParentRefs(parentRefs []gatewayv1.ParentReference, gateways map[string]struct{}) []gatewayv1.ParentReference {
	var out []gatewayv1.ParentReference
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		if _, ok := gateways[string(p.Name)]; !ok {
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

func (r *Reconciler) backendsResolveHTTP(ctx context.Context, ns string, rules []gatewayv1.HTTPRouteRule) (bool, string, string) {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return r.backendsResolveL4(ctx, ns, refs)
}

func (r *Reconciler) backendsResolveTLS(ctx context.Context, ns string, rules []gatewayv1.TLSRouteRule) (bool, string, string) {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return r.backendsResolveL4(ctx, ns, refs)
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
func (r *Reconciler) backendsResolveL4(_ context.Context, _ string, refs []gatewayv1.BackendObjectReference) (bool, string, string) {
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
