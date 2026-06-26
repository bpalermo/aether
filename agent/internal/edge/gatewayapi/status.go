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

// writeGatewayClassStatus sets Accepted=True and the supportedFeatures list on
// EVERY GatewayClass whose controllerName is the edge controller (not only the
// configured GatewayClassName). The Accepted condition carries the class's own
// metadata.generation as observedGeneration, so a spec change that bumps the
// generation re-publishes an in-sync observedGeneration (the
// GatewayClassObservedGenerationBump conformance test). It is a no-op for any
// class owned by another controller (we MUST only touch classes we control).
// supportedFeatures is what the upstream conformance suite reads to decide which
// test suites to run vs skip.
func (r *Reconciler) writeGatewayClassStatus(ctx context.Context) {
	list := &gatewayv1.GatewayClassList{}
	// Uncached read: GatewayClass is cluster-scoped and a cached List can race the
	// informer sync on the first reconcile.
	if err := r.APIReader.List(ctx, list); err != nil {
		r.Log.WarnContext(ctx, "failed to list GatewayClasses for status", "error", err.Error())
		return
	}
	for i := range list.Items {
		gc := &list.Items[i]
		if gc.Spec.ControllerName != gatewaystatus.EdgeControllerName {
			continue
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
			continue
		}
		gc.Status.Conditions = conds
		gc.Status.SupportedFeatures = features
		if err := r.Status().Update(ctx, gc); err != nil {
			r.Log.WarnContext(ctx, "failed to write GatewayClass status", "gatewayClass", gc.Name, "error", err.Error())
		}
	}
}

// writeGatewayStatuses sets, for each of our Gateways, top-level Accepted=True +
// Programmed=True, a per-listener status (supportedKinds, attachedRoutes,
// Programmed/Accepted/ResolvedRefs=True), and status.addresses.
//
// When perGWAssignedIPs is non-nil (proposal 021 Phase 2), each Gateway gets its
// own per-Gateway LB IP from that map. Otherwise (Phase 1), every Gateway gets the
// shared edge LB IP from EdgeServiceName. attachedRoutes is computed from the
// routes the reconciler already listed.
func (r *Reconciler) writeGatewayStatuses(
	ctx context.Context,
	ourGateways []gatewayv1.Gateway,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
	tlsResults map[listenerStatusKey]listenerTLSResult,
	perGWAssignedIPs map[gatewayKey]string,
) {
	// Proposal 021 Phase 1 fallback: resolve the shared edge LoadBalancer address
	// once and publish it on every class-aether Gateway when Phase 2 is not active.
	var sharedAddrs []gatewayv1.GatewayStatusAddress
	if len(perGWAssignedIPs) == 0 {
		sharedAddrs = r.resolveGatewayAddresses(ctx)
	}
	for i := range ourGateways {
		gw := &ourGateways[i]
		desired := *gw.DeepCopy()

		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}
		listeners := make([]gatewayv1.ListenerStatus, 0, len(gw.Spec.Listeners))
		allListenersProgrammed := true
		for _, ln := range gw.Spec.Listeners {
			attached := r.attachedRoutesForListener(ctx, gw, ln, httpRoutes, tcpRoutes, tlsRoutes, gateways, listenerKeys)

			// ResolvedRefs: InvalidRouteKinds (allowedRoutes.kinds names an
			// unsupported kind) takes precedence; then InvalidCertificateRef
			// (a TLS listener whose certificateRefs don't resolve).
			resolvedStatus := metav1.ConditionTrue
			resolvedReason := string(gatewayv1.ListenerReasonResolvedRefs)
			resolvedMsg := "All listener references resolved"
			programmed := true
			switch {
			case listenerHasInvalidRouteKinds(ln):
				resolvedStatus = metav1.ConditionFalse
				resolvedReason = string(gatewayv1.ListenerReasonInvalidRouteKinds)
				resolvedMsg = "allowedRoutes.kinds names a Route kind this listener does not support"
				programmed = false
			default:
				if res, ok := tlsResults[listenerStatusKey{Gateway: gk, Name: ln.Name}]; ok && res.hasTLS && !res.resolved {
					resolvedStatus = metav1.ConditionFalse
					resolvedReason = string(gatewayv1.ListenerReasonInvalidCertificateRef)
					if res.reason != "" {
						resolvedReason = res.reason
					}
					resolvedMsg = res.message
					programmed = false
				}
			}

			acceptedStatus := metav1.ConditionTrue
			acceptedReason := string(gatewayv1.ListenerReasonAccepted)
			acceptedMsg := "Listener accepted"

			programmedStatus := metav1.ConditionTrue
			programmedReason := string(gatewayv1.ListenerReasonProgrammed)
			programmedMsg := "Listener programmed"
			if !programmed {
				programmedStatus = metav1.ConditionFalse
				programmedReason = string(gatewayv1.ListenerReasonInvalid)
				programmedMsg = "Listener not programmed: " + resolvedMsg
				allListenersProgrammed = false
			}

			lc, _ := gatewaystatus.MergeConditions(existingListenerConditions(gw.Status.Listeners, ln.Name), gw.Generation,
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionAccepted),
					Status:  acceptedStatus,
					Reason:  acceptedReason,
					Message: acceptedMsg,
				},
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionProgrammed),
					Status:  programmedStatus,
					Reason:  programmedReason,
					Message: programmedMsg,
				},
				gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  resolvedStatus,
					Reason:  resolvedReason,
					Message: resolvedMsg,
				},
			)
			listeners = append(listeners, gatewayv1.ListenerStatus{
				Name:           ln.Name,
				SupportedKinds: listenerSupportedKinds(ln),
				AttachedRoutes: attached,
				Conditions:     lc,
			})
		}

		// Top-level Programmed is False when any listener failed to program (e.g.
		// invalid TLS or invalid route kinds), so the Gateway is not advertised as
		// fully programmed while a listener is broken.
		programmedCond := gatewaystatus.Condition{
			Type:    string(gatewayv1.GatewayConditionProgrammed),
			Status:  metav1.ConditionTrue,
			Reason:  string(gatewayv1.GatewayReasonProgrammed),
			Message: "Gateway programmed into the aether edge data plane",
		}
		if !allListenersProgrammed {
			programmedCond.Status = metav1.ConditionFalse
			programmedCond.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			programmedCond.Message = "One or more listeners are not programmed (invalid TLS or route kinds)"
		}
		conds, topChanged := gatewaystatus.MergeConditions(desired.Status.Conditions, gw.Generation,
			gatewaystatus.Condition{
				Type:    string(gatewayv1.GatewayConditionAccepted),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.GatewayReasonAccepted),
				Message: "Gateway accepted by the aether edge controller",
			},
			programmedCond,
		)
		desired.Status.Conditions = conds

		listenersChanged := !listenerStatusesEqual(gw.Status.Listeners, listeners)
		desired.Status.Listeners = listeners

		// Publish status.addresses: Phase 2 uses the per-Gateway LB IP; Phase 1
		// uses the shared edge LB IP. Never overwrite a previously-published address
		// with an empty list — leave the old address in place and wait for the next
		// reconcile once MetalLB assigns an IP.
		addrsChanged := false
		var addrs []gatewayv1.GatewayStatusAddress
		if ip, ok := perGWAssignedIPs[gk]; ok && ip != "" {
			// Phase 2: per-Gateway IP from the per-Gateway LoadBalancer Service.
			addrs = []gatewayv1.GatewayStatusAddress{{
				Type:  ptr(gatewayv1.IPAddressType),
				Value: ip,
			}}
		} else {
			// Phase 1: shared edge LB IP.
			addrs = sharedAddrs
		}
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
	ctx context.Context,
	gw *gatewayv1.Gateway,
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
			if r.httpRouteAttachesToListener(ctx, gw, hr.Spec.ParentRefs, hr.Namespace, hr.Spec.Hostnames, ln) {
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

// httpRouteAttachesToListener reports whether an HTTPRoute (in routeNamespace, with
// routeHostnames) attaches to the given gateway listener. A parentRef with no
// sectionName/port may attach to every HTTP/HTTPS listener; a sectionName or port
// narrows it. Attachment additionally requires the listener to admit the route's
// namespace (allowedRoutes.namespaces), accept HTTPRoute (allowedRoutes.kinds),
// and the route's hostnames to intersect the listener hostname. The parentRef
// namespace defaults to the route's namespace.
func (r *Reconciler) httpRouteAttachesToListener(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
	routeHostnames []gatewayv1.Hostname,
	ln gatewayv1.Listener,
) bool {
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		if string(p.Name) != gw.Name || parentRefNamespace(p.Namespace, routeNamespace) != gw.Namespace {
			continue
		}
		if !parentRefSelectsListener(p, ln) {
			continue
		}
		if !listenerAcceptsKind(ln, "HTTPRoute") {
			continue
		}
		if !r.namespaceAllowed(ctx, gw.Namespace, routeNamespace, ln) {
			continue
		}
		if !hostnamesIntersect(ln.Hostname, routeHostnames) {
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
	ourGateways []gatewayv1.Gateway,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1alpha2.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
	grants []gatewayv1beta1.ReferenceGrant,
) {
	gwByKey := map[gatewayKey]*gatewayv1.Gateway{}
	for i := range ourGateways {
		gwByKey[gatewayKey{Namespace: ourGateways[i].Namespace, Name: ourGateways[i].Name}] = &ourGateways[i]
	}
	for i := range httpRoutes {
		hr := &httpRoutes[i]
		// Acceptance is per parentRef: a route may attach to one Gateway and be
		// rejected by another. We resolve the most specific reason against each of
		// our Gateways via httpRouteAttachment.
		accepted, acceptedReason, acceptedMsg := r.httpRouteAcceptance(ctx, hr, gwByKey)
		resolved, reason, msg := r.backendsResolveHTTP(ctx, hr.Namespace, hr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, ourGatewayParentRefs(hr.Spec.ParentRefs, hr.Namespace, gateways), accepted, acceptedReason, acceptedMsg, resolved, reason, msg, "HTTPRoute")
	}
	for i := range tcpRoutes {
		tr := &tcpRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, listenerKeys)) > 0
		ar, am := acceptedReasonMsg(accepted)
		resolved, reason, msg := r.backendsResolveL4(ctx, tr.Namespace, "TCPRoute", l4BackendObjectRefs(tr.Spec.Rules), grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, ar, am, resolved, reason, msg, "TCPRoute")
	}
	for i := range tlsRoutes {
		tr := &tlsRoutes[i]
		accepted := len(gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, listenerKeys)) > 0
		ar, am := acceptedReasonMsg(accepted)
		resolved, reason, msg := r.backendsResolveTLS(ctx, tr.Namespace, tr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, ourGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, ar, am, resolved, reason, msg, "TLSRoute")
	}
}

// httpRouteAcceptance computes the route-level Accepted condition for an HTTPRoute
// across all of our Gateways it references. The route is Accepted when it attaches
// to at least one listener of at least one of our Gateways; otherwise the most
// specific failure reason wins (NoMatchingListenerHostname > NotAllowedByListeners
// > NoMatchingParent).
func (r *Reconciler) httpRouteAcceptance(ctx context.Context, hr *gatewayv1.HTTPRoute, gwByKey map[gatewayKey]*gatewayv1.Gateway) (bool, string, string) {
	best := attachNoMatchingParent
	for _, p := range hr.Spec.ParentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := gatewayKey{Namespace: parentRefNamespace(p.Namespace, hr.Namespace), Name: string(p.Name)}
		gw, ok := gwByKey[key]
		if !ok {
			continue
		}
		switch r.httpRouteAttachment(ctx, gw, []gatewayv1.ParentReference{p}, hr.Namespace, hr.Spec.Hostnames) {
		case attachAccepted:
			return true, string(gatewayv1.RouteReasonAccepted), "Route accepted by the aether edge controller"
		case attachNoMatchingListenerHostname:
			best = attachNoMatchingListenerHostname
		case attachNotAllowedByListeners:
			if best != attachNoMatchingListenerHostname {
				best = attachNotAllowedByListeners
			}
		}
	}
	switch best {
	case attachNoMatchingListenerHostname:
		return false, string(gatewayv1.RouteReasonNoMatchingListenerHostname), "No listener hostname intersects the route hostnames"
	case attachNotAllowedByListeners:
		return false, string(gatewayv1.RouteReasonNotAllowedByListeners), "Route namespace is not allowed by the listener allowedRoutes.namespaces"
	default:
		return false, string(gatewayv1.RouteReasonNoMatchingParent), "Route does not attach to a matching listener on this Gateway"
	}
}

// acceptedReasonMsg returns the Accepted reason/message for a simple
// (TCP/TLS) attach boolean.
func acceptedReasonMsg(accepted bool) (string, string) {
	if accepted {
		return string(gatewayv1.RouteReasonAccepted), "Route accepted by the aether edge controller"
	}
	return string(gatewayv1.RouteReasonNoMatchingParent), "Route does not attach to a matching listener on this Gateway"
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
	accepted bool,
	acceptedReason, acceptedMsg string,
	backendsResolved bool,
	resolvedReason, resolvedMsg, kind string,
) {
	if len(parentRefs) == 0 {
		return
	}
	parents := status.Parents
	changed := false
	acceptedStatus := metav1.ConditionTrue
	if !accepted {
		acceptedStatus = metav1.ConditionFalse
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

// backendsResolveL4 reports whether every backendRef is resolvable, per the Gateway
// API ResolvedRefs semantics. A backendRef is resolved when (in precedence order):
//   - its group/kind is the core ("") Service kind — otherwise InvalidKind;
//   - it has a non-empty name — otherwise BackendNotFound (empty name);
//   - if cross-namespace, a ReferenceGrant permits it — otherwise RefNotPermitted;
//   - the referenced backend EXISTS — either as a mesh/registry service (checked
//     first, namespace-blind) or as a real k8s Service — otherwise BackendNotFound.
//
// Existence uses the registry-aware backendExists helper: a backend is resolved
// when the registry's ServiceCatalog knows the bare service name (mesh backend,
// no k8s Service required in the route namespace), or when a k8s Service Get
// succeeds. This prevents mesh-backed edge routes from being incorrectly marked
// BackendNotFound when the route namespace differs from the service's k8s namespace.
// Across the refs in a rule the first/most-specific failure wins, with InvalidKind
// taking precedence over BackendNotFound (it short-circuits before the existence check).
func (r *Reconciler) backendsResolveL4(ctx context.Context, routeNamespace, routeKind string, refs []gatewayv1.BackendObjectReference, grants []gatewayv1beta1.ReferenceGrant) (bool, string, string) {
	for _, ref := range refs {
		if (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			return false, string(gatewayv1.RouteReasonInvalidKind), fmt.Sprintf("backendRef %q is not a core Service", ref.Name)
		}
		if string(ref.Name) == "" {
			return false, string(gatewayv1.RouteReasonBackendNotFound), "backendRef has an empty name"
		}
		backendNS := derefBackendNamespace(ref.Namespace)
		if referencegrant.CrossNamespace(backendNS, routeNamespace) &&
			!referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, backendNS, string(ref.Name)) {
			return false, string(gatewayv1.RouteReasonRefNotPermitted),
				fmt.Sprintf("cross-namespace backendRef to Service %q in namespace %q is not permitted by any ReferenceGrant", ref.Name, backendNS)
		}
		// Registry-aware existence check: a mesh/registry backend resolves even when
		// there is no k8s Service in the route namespace (registry is namespace-blind).
		// Falls back to k8s Service Get for cleartext edge backends.
		svcNS := backendNS
		if svcNS == "" {
			svcNS = routeNamespace
		}
		if !r.backendExists(ctx, string(ref.Name), svcNS) {
			return false, string(gatewayv1.RouteReasonBackendNotFound),
				fmt.Sprintf("backendRef Service %q in namespace %q does not exist", ref.Name, svcNS)
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"
}
