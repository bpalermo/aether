package gatewayapi

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/edge/gatewayapi/attachment"
	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/common/referencegrant"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	tcpRoutes []gatewayv1.TCPRoute,
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
	att := &attachment.Resolver{Reader: r.Client, Log: r.Log}
	for i := range ourGateways {
		gw := &ourGateways[i]
		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}

		// Compute the SPEC-derived desired status once. These inputs depend on the
		// Gateway spec + the routes/certs/assigned-IP we resolved this reconcile —
		// NOT on the Gateway's current status — so they're stable across the
		// conflict-retry re-Gets in writeGatewayStatus below.
		listenerInputs := make([]listenerStatusInput, 0, len(gw.Spec.Listeners))
		allListenersProgrammed := true
		anyListenerAccepted := false
		anyUnsupportedProtocol := false
		for _, ln := range gw.Spec.Listeners {
			// Unsupported listener protocol (gateway-api 1.6's
			// GatewayListenerUnsupportedProtocol contract): the listener is not
			// accepted (UnsupportedProtocol), publishes empty supportedKinds, and
			// counts against the Gateway's Accepted/Programmed rollup below.
			if attachment.SupportedKindsFor(ln.Protocol) == nil {
				// NOTE: deliberately NOT counted against allListenersProgrammed —
				// the suite's readiness poller requires top-level Programmed=True
				// on the mixed (supported+unsupported) fixture Gateway; only a
				// Gateway with NO accepted listener rolls up Programmed=False.
				anyUnsupportedProtocol = true
				msg := fmt.Sprintf("Listener protocol %q is not supported by the aether edge (supported: HTTP, HTTPS, TLS, TCP)", ln.Protocol)
				listenerInputs = append(listenerInputs, listenerStatusInput{
					name:           ln.Name,
					supportedKinds: []gatewayv1.RouteGroupKind{},
					attached:       0,
					accepted: gatewaystatus.Condition{
						Type:    string(gatewayv1.ListenerConditionAccepted),
						Status:  metav1.ConditionFalse,
						Reason:  string(gatewayv1.ListenerReasonUnsupportedProtocol),
						Message: msg,
					},
					programmed: gatewaystatus.Condition{
						Type:    string(gatewayv1.ListenerConditionProgrammed),
						Status:  metav1.ConditionFalse,
						Reason:  string(gatewayv1.ListenerReasonInvalid),
						Message: msg,
					},
					resolved: gatewaystatus.Condition{
						Type:    string(gatewayv1.ListenerConditionResolvedRefs),
						Status:  metav1.ConditionTrue,
						Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
						Message: "No references to resolve",
					},
				})
				continue
			}
			anyListenerAccepted = true
			attached := att.AttachedRoutesForListener(ctx, gw, ln, httpRoutes, tcpRoutes, tlsRoutes, gateways, listenerKeys)

			// ResolvedRefs: InvalidRouteKinds (allowedRoutes.kinds names an
			// unsupported kind) takes precedence; then InvalidCertificateRef
			// (a TLS listener whose certificateRefs don't resolve).
			resolvedStatus := metav1.ConditionTrue
			resolvedReason := string(gatewayv1.ListenerReasonResolvedRefs)
			resolvedMsg := "All listener references resolved"
			programmed := true
			switch {
			case attachment.ListenerHasInvalidRouteKinds(ln):
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

			programmedStatus := metav1.ConditionTrue
			programmedReason := string(gatewayv1.ListenerReasonProgrammed)
			programmedMsg := "Listener programmed"
			if !programmed {
				programmedStatus = metav1.ConditionFalse
				programmedReason = string(gatewayv1.ListenerReasonInvalid)
				programmedMsg = "Listener not programmed: " + resolvedMsg
				allListenersProgrammed = false
			}

			listenerInputs = append(listenerInputs, listenerStatusInput{
				name:           ln.Name,
				supportedKinds: attachment.ListenerSupportedKinds(ln),
				attached:       attached,
				accepted: gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionAccepted),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonAccepted),
					Message: "Listener accepted",
				},
				programmed: gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionProgrammed),
					Status:  programmedStatus,
					Reason:  programmedReason,
					Message: programmedMsg,
				},
				resolved: gatewaystatus.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  resolvedStatus,
					Reason:  resolvedReason,
					Message: resolvedMsg,
				},
			})
		}

		// Top-level Programmed is False when any listener failed to program (e.g.
		// invalid TLS or invalid route kinds), so the Gateway is not advertised as
		// fully programmed while a listener is broken.
		programmedTop := gatewaystatus.Condition{
			Type:    string(gatewayv1.GatewayConditionProgrammed),
			Status:  metav1.ConditionTrue,
			Reason:  string(gatewayv1.GatewayReasonProgrammed),
			Message: "Gateway programmed into the aether edge data plane",
		}
		if !allListenersProgrammed {
			programmedTop.Status = metav1.ConditionFalse
			programmedTop.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			programmedTop.Message = "One or more listeners are not programmed (invalid TLS or route kinds)"
		}
		if !anyListenerAccepted && len(gw.Spec.Listeners) > 0 {
			// All listeners rejected (unsupported protocols): nothing programs.
			programmedTop.Status = metav1.ConditionFalse
			programmedTop.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			programmedTop.Message = "No listener is accepted (unsupported protocols)"
		}
		acceptedTop := gatewaystatus.Condition{
			Type:    string(gatewayv1.GatewayConditionAccepted),
			Status:  metav1.ConditionTrue,
			Reason:  string(gatewayv1.GatewayReasonAccepted),
			Message: "Gateway accepted by the aether edge controller",
		}
		// Listener-protocol rollup (1.6 contract): no accepted listener at all →
		// Accepted=False/ListenersNotValid; some unsupported but ≥1 accepted →
		// Accepted stays True with reason ListenersNotValid.
		switch {
		case !anyListenerAccepted && len(gw.Spec.Listeners) > 0:
			acceptedTop.Status = metav1.ConditionFalse
			acceptedTop.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			acceptedTop.Message = "No listener is accepted (unsupported protocols)"
		case anyUnsupportedProtocol:
			acceptedTop.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			acceptedTop.Message = "Gateway accepted; some listeners are not valid (unsupported protocols)"
		}
		// An infrastructure.parametersRef that is present but unresolvable rejects
		// the Gateway (Accepted=False/InvalidParameters — Gateway API spec, pinned
		// by the GatewayInvalidParametersRef conformance test). The data plane
		// stays tolerant (listeners keep serving on compiled defaults) so a
		// briefly-dangling EdgeConfig doesn't drop live traffic; only the
		// advertised status reflects the invalid ref.
		if msg, invalid := r.invalidParametersRef(ctx, gw); invalid {
			acceptedTop.Status = metav1.ConditionFalse
			acceptedTop.Reason = string(gatewayv1.GatewayReasonInvalidParameters)
			acceptedTop.Message = msg
			programmedTop.Status = metav1.ConditionFalse
			programmedTop.Reason = string(gatewayv1.GatewayReasonInvalid)
			programmedTop.Message = msg
		}

		// status.addresses: Phase 2 uses the per-Gateway LB IP; Phase 1 uses the
		// shared edge LB IP. An empty list never overwrites a previously-published
		// address (writeGatewayStatus leaves the old one and waits for the next
		// reconcile once MetalLB assigns an IP).
		var addrs []gatewayv1.GatewayStatusAddress
		if ip, ok := perGWAssignedIPs[gk]; ok && ip != "" {
			addrs = []gatewayv1.GatewayStatusAddress{{
				Type:  ptr(gatewayv1.IPAddressType),
				Value: ip,
			}}
		} else {
			addrs = sharedAddrs
		}

		key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
		if err := r.writeGatewayStatus(ctx, key, listenerInputs, acceptedTop, programmedTop, addrs); err != nil {
			r.Log.WarnContext(ctx, "failed to write Gateway status", "gateway", gw.Name, "error", err.Error())
		}
	}
}

// listenerStatusInput holds the spec-derived inputs for one listener's status,
// computed once per reconcile and merged against the Gateway's *latest* status
// inside the conflict-retry loop in writeGatewayStatus.
type listenerStatusInput struct {
	name           gatewayv1.SectionName
	supportedKinds []gatewayv1.RouteGroupKind
	attached       int32
	accepted       gatewaystatus.Condition
	programmed     gatewaystatus.Condition
	resolved       gatewaystatus.Condition
}

// writeGatewayStatus merges the computed desired status onto the LATEST version
// of the Gateway and writes it, retrying on optimistic-lock conflicts.
//
// Both edge replicas reconcile the same cluster-wide Gateways active-active, so
// their status writes collide with 409 Conflict. Dropping the losing write (the
// previous behavior) let a Gateway sit address-less past the conformance
// GatewayMustHaveAddress window under churn — the HTTPRouteRedirectPortAndScheme
// timeout. RetryOnConflict re-Gets and re-merges; because the desired status is a
// deterministic function of spec, a re-applied identical status reports no change
// and the write converges without inter-replica lastTransitionTime flapping.
func (r *Reconciler) writeGatewayStatus(
	ctx context.Context,
	key types.NamespacedName,
	listenerInputs []listenerStatusInput,
	acceptedTop, programmedTop gatewaystatus.Condition,
	addrs []gatewayv1.GatewayStatusAddress,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gw := &gatewayv1.Gateway{}
		if err := r.Get(ctx, key, gw); err != nil {
			if apierrors.IsNotFound(err) {
				// Gateway deleted mid-reconcile; nothing to write.
				return nil
			}
			return err
		}

		listeners := make([]gatewayv1.ListenerStatus, 0, len(listenerInputs))
		for _, in := range listenerInputs {
			lc, _ := gatewaystatus.MergeConditions(
				existingListenerConditions(gw.Status.Listeners, in.name), gw.Generation,
				in.accepted, in.programmed, in.resolved,
			)
			listeners = append(listeners, gatewayv1.ListenerStatus{
				Name:           in.name,
				SupportedKinds: in.supportedKinds,
				AttachedRoutes: in.attached,
				Conditions:     lc,
			})
		}

		conds, topChanged := gatewaystatus.MergeConditions(gw.Status.Conditions, gw.Generation, acceptedTop, programmedTop)
		listenersChanged := !listenerStatusesEqual(gw.Status.Listeners, listeners)
		addrsChanged := len(addrs) > 0 && !gatewayAddressesEqual(gw.Status.Addresses, addrs)

		if !topChanged && !listenersChanged && !addrsChanged {
			return nil
		}

		gw.Status.Conditions = conds
		gw.Status.Listeners = listeners
		if len(addrs) > 0 {
			gw.Status.Addresses = addrs
		}
		return r.Status().Update(ctx, gw)
	})
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
	tcpRoutes []gatewayv1.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[gatewayKey]struct{},
	listenerKeys map[gatewayListenerKey]struct{},
	grants []gatewayv1beta1.ReferenceGrant,
) {
	gwByKey := map[gatewayKey]*gatewayv1.Gateway{}
	for i := range ourGateways {
		gwByKey[gatewayKey{Namespace: ourGateways[i].Namespace, Name: ourGateways[i].Name}] = &ourGateways[i]
	}
	att := &attachment.Resolver{Reader: r.Client, Log: r.Log}
	for i := range httpRoutes {
		hr := &httpRoutes[i]
		// Acceptance is per parentRef: a route may attach to one Gateway and be
		// rejected by another. We resolve the most specific reason against each of
		// our Gateways via the attachment resolver.
		accepted, acceptedReason, acceptedMsg := att.HTTPRouteAcceptance(ctx, hr, gwByKey)
		resolved, reason, msg := r.backendsResolveHTTP(ctx, hr.Namespace, hr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, hr, hr.Generation, &hr.Status.RouteStatus, attachment.OurGatewayParentRefs(hr.Spec.ParentRefs, hr.Namespace, gateways), accepted, acceptedReason, acceptedMsg, resolved, reason, msg, "HTTPRoute")
	}
	for i := range tcpRoutes {
		tr := &tcpRoutes[i]
		accepted := len(attachment.GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, listenerKeys)) > 0
		ar, am := acceptedReasonMsg(accepted)
		resolved, reason, msg := r.backendsResolveL4(ctx, tr.Namespace, "TCPRoute", l4BackendObjectRefs(tr.Spec.Rules), grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, attachment.OurGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, ar, am, resolved, reason, msg, "TCPRoute")
	}
	for i := range tlsRoutes {
		tr := &tlsRoutes[i]
		accepted := len(attachment.GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, listenerKeys)) > 0
		ar, am := acceptedReasonMsg(accepted)
		resolved, reason, msg := r.backendsResolveTLS(ctx, tr.Namespace, tr.Spec.Rules, grants)
		r.writeRouteParentStatus(ctx, tr, tr.Generation, &tr.Status.RouteStatus, attachment.OurGatewayParentRefs(tr.Spec.ParentRefs, tr.Namespace, gateways), accepted, ar, am, resolved, reason, msg, "TLSRoute")
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
		parents, upd = gatewaystatus.MergeRouteParentStatus(
			parents, gatewaystatus.EdgeControllerName, p, generation,
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

func l4BackendObjectRefs(rules []gatewayv1.TCPRouteRule) []gatewayv1.BackendObjectReference {
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
		backendNS := attachment.DerefBackendNamespace(ref.Namespace)
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
