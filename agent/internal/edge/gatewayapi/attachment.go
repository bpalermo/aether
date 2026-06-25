package gatewayapi

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// routeAttachReason enumerates why a route does (Accepted) or does not attach to
// any listener of a Gateway. The zero value (attachAccepted) means it attaches.
type routeAttachReason int

const (
	// attachAccepted: the route attaches to at least one listener.
	attachAccepted routeAttachReason = iota
	// attachNoMatchingParent: the parentRef names a sectionName/port that no
	// listener on the Gateway has.
	attachNoMatchingParent
	// attachNotAllowedByListeners: a candidate listener exists, but its
	// allowedRoutes.namespaces excludes the route's namespace (or no candidate
	// listener allowed the route's namespace).
	attachNotAllowedByListeners
	// attachNoMatchingListenerHostname: a candidate listener exists and allows the
	// namespace, but the route's hostnames don't intersect the listener hostname.
	attachNoMatchingListenerHostname
)

// listenerAcceptsKind reports whether a listener (by protocol + allowedRoutes.kinds)
// accepts the given route kind. allowedRoutes.kinds, when set, restricts the
// otherwise protocol-default supported kinds; a kind not in the protocol default
// is never accepted.
func listenerAcceptsKind(ln gatewayv1.Listener, routeKind string) bool {
	for _, s := range listenerSupportedKinds(ln) {
		if string(s.Kind) == routeKind {
			return true
		}
	}
	return false
}

// listenerSupportedKinds returns the supportedKinds to publish on a listener's
// status: the protocol-default kinds, intersected with allowedRoutes.kinds when
// the listener sets one. A listener whose allowedRoutes.kinds names only
// unsupported kinds yields an empty (non-nil) slice, matching the upstream
// conformance expectation (supportedKinds: []).
func listenerSupportedKinds(ln gatewayv1.Listener) []gatewayv1.RouteGroupKind {
	defaults := supportedKindsFor(ln.Protocol)
	if ln.AllowedRoutes == nil || len(ln.AllowedRoutes.Kinds) == 0 {
		return defaults
	}
	out := make([]gatewayv1.RouteGroupKind, 0, len(defaults))
	for _, want := range ln.AllowedRoutes.Kinds {
		for _, d := range defaults {
			if d.Kind == want.Kind && groupEqual(d.Group, want.Group) {
				out = append(out, d)
			}
		}
	}
	return out
}

// listenerHasInvalidRouteKinds reports whether a listener's allowedRoutes.kinds
// names any kind the listener's protocol does not support (drives the
// ResolvedRefs=False / InvalidRouteKinds listener condition).
func listenerHasInvalidRouteKinds(ln gatewayv1.Listener) bool {
	if ln.AllowedRoutes == nil || len(ln.AllowedRoutes.Kinds) == 0 {
		return false
	}
	defaults := supportedKindsFor(ln.Protocol)
	for _, want := range ln.AllowedRoutes.Kinds {
		ok := false
		for _, d := range defaults {
			if d.Kind == want.Kind && groupEqual(d.Group, want.Group) {
				ok = true
				break
			}
		}
		if !ok {
			return true
		}
	}
	return false
}

func groupEqual(a, b *gatewayv1.Group) bool {
	ag, bg := gatewayv1.Group(""), gatewayv1.Group("")
	if a != nil {
		ag = *a
	}
	if b != nil {
		bg = *b
	}
	// An empty group on a RouteGroupKind defaults to the Gateway API group.
	if ag == "" {
		ag = gatewayv1.Group(gatewayv1.GroupName)
	}
	if bg == "" {
		bg = gatewayv1.Group(gatewayv1.GroupName)
	}
	return ag == bg
}

// parentRefSelectsListener reports whether a parentRef (already known to target
// this Gateway) selects the given listener by sectionName and/or port. An unset
// sectionName/port selects every listener; a set one must match.
func parentRefSelectsListener(p gatewayv1.ParentReference, ln gatewayv1.Listener) bool {
	if p.SectionName != nil && *p.SectionName != ln.Name {
		return false
	}
	if p.Port != nil && uint32(*p.Port) != uint32(ln.Port) {
		return false
	}
	return true
}

// parentRefNamesAnyListener reports whether the parentRef's sectionName/port (if
// set) matches at least one listener on the Gateway. A parentRef naming a
// sectionName/port that no listener has yields NoMatchingParent.
func parentRefNamesAnyListener(p gatewayv1.ParentReference, listeners []gatewayv1.Listener) bool {
	for _, ln := range listeners {
		if parentRefSelectsListener(p, ln) {
			return true
		}
	}
	return false
}

// namespaceAllowed reports whether a listener's allowedRoutes.namespaces admits a
// route in routeNamespace. The Gateway lives in gwNamespace. Selector matching
// fetches the route namespace's labels via the client; a fetch failure is treated
// as "not allowed" (fail-closed) rather than panicking.
func (r *Reconciler) namespaceAllowed(ctx context.Context, gwNamespace, routeNamespace string, ln gatewayv1.Listener) bool {
	from := gatewayv1.NamespacesFromSame
	var selector *metav1.LabelSelector
	if ln.AllowedRoutes != nil && ln.AllowedRoutes.Namespaces != nil {
		ns := ln.AllowedRoutes.Namespaces
		if ns.From != nil {
			from = *ns.From
		}
		selector = ns.Selector
	}
	switch from {
	case gatewayv1.NamespacesFromAll:
		return true
	case gatewayv1.NamespacesFromNone:
		return false
	case gatewayv1.NamespacesFromSelector:
		if selector == nil {
			return false
		}
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return false
		}
		nsObj := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: routeNamespace}, nsObj); err != nil {
			if !apierrors.IsNotFound(err) {
				r.Log.WarnContext(ctx, "failed to get namespace for allowedRoutes selector",
					"namespace", routeNamespace, "error", err.Error())
			}
			return false
		}
		return sel.Matches(labels.Set(nsObj.Labels))
	default: // Same
		return routeNamespace == gwNamespace
	}
}

// hostnamesIntersect reports whether a route's hostnames intersect a listener's
// hostname. An empty listener hostname matches any route hostname; an empty route
// hostname set matches any listener hostname. Wildcard (*.suffix) listener
// hostnames match by suffix. This mirrors the Gateway API hostname-intersection
// rule used to drive NoMatchingListenerHostname.
func hostnamesIntersect(listenerHostname *gatewayv1.Hostname, routeHostnames []gatewayv1.Hostname) bool {
	if listenerHostname == nil || *listenerHostname == "" {
		return true
	}
	if len(routeHostnames) == 0 {
		return true
	}
	lh := string(*listenerHostname)
	for _, rh := range routeHostnames {
		if hostnameMatch(lh, string(rh)) {
			return true
		}
	}
	return false
}

// hostnameMatch reports whether a listener hostname and a route hostname overlap,
// honoring wildcard prefixes on either side.
func hostnameMatch(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasPrefix(a, "*.") {
		if strings.HasSuffix(b, a[1:]) && b != a[1:] {
			return true
		}
		if strings.HasPrefix(b, "*.") {
			return strings.HasSuffix(a, b[1:]) || strings.HasSuffix(b, a[1:])
		}
	}
	if strings.HasPrefix(b, "*.") {
		return strings.HasSuffix(a, b[1:]) && a != b[1:]
	}
	return false
}

// httpRouteAttachment computes whether an HTTPRoute attaches to the named Gateway
// and, if not, why. It honors: parentRef sectionName/port (NoMatchingParent),
// allowedRoutes.namespaces (NotAllowedByListeners), allowedRoutes.kinds, and
// listener-hostname intersection (NoMatchingListenerHostname). A route attaches
// when at least one listener of the Gateway accepts it on all axes.
func (r *Reconciler) httpRouteAttachment(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
	routeHostnames []gatewayv1.Hostname,
) routeAttachReason {
	reason := attachNoMatchingParent
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		if string(p.Name) != gw.Name || parentRefNamespace(p.Namespace, routeNamespace) != gw.Namespace {
			continue
		}
		// The parentRef targets this Gateway. Does its sectionName/port name a
		// real listener?
		if !parentRefNamesAnyListener(p, gw.Spec.Listeners) {
			// NoMatchingParent unless a better (already-found) reason exists.
			continue
		}
		// Walk the listeners this parentRef selects, narrowing the failure reason
		// from namespace → hostname as we get closer to a match.
		for _, ln := range gw.Spec.Listeners {
			if !parentRefSelectsListener(p, ln) {
				continue
			}
			if !listenerAcceptsKind(ln, "HTTPRoute") {
				continue
			}
			if !r.namespaceAllowed(ctx, gw.Namespace, routeNamespace, ln) {
				if reason == attachNoMatchingParent {
					reason = attachNotAllowedByListeners
				}
				continue
			}
			if !hostnamesIntersect(ln.Hostname, routeHostnames) {
				reason = attachNoMatchingListenerHostname
				continue
			}
			return attachAccepted
		}
		// This parentRef named a real listener but none accepted; keep the most
		// specific reason found so far (namespace/hostname over NoMatchingParent).
	}
	return reason
}
