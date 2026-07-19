// Package attachment holds the edge Gateway API controller's route→listener
// attachment resolution: parentRef matching, allowedRoutes (namespaces/kinds)
// admission, ReferenceGrant-gated backendRef admission, and listener/route
// hostname intersection. It is deliberately free of any xDS proxy/cache
// dependency so the attachment semantics (and their tests) stand alone from
// Envoy resource generation; the gatewayapi reconciler composes it.
package attachment

import (
	"context"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Resolver resolves route→listener attachment. It needs only a Kubernetes
// reader (to fetch route-namespace labels for allowedRoutes.namespaces
// selectors) and a logger — no xDS proxy/cache types.
type Resolver struct {
	client.Reader
	Log *slog.Logger
}

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
	for _, s := range ListenerSupportedKinds(ln) {
		if string(s.Kind) == routeKind {
			return true
		}
	}
	return false
}

// ListenerSupportedKinds returns the supportedKinds to publish on a listener's
// status: the protocol-default kinds, intersected with allowedRoutes.kinds when
// the listener sets one. A listener whose allowedRoutes.kinds names only
// unsupported kinds yields an empty (non-nil) slice, matching the upstream
// conformance expectation (supportedKinds: []).
func ListenerSupportedKinds(ln gatewayv1.Listener) []gatewayv1.RouteGroupKind {
	defaults := SupportedKindsFor(ln.Protocol)
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

// ListenerHasInvalidRouteKinds reports whether a listener's allowedRoutes.kinds
// names any kind the listener's protocol does not support (drives the
// ResolvedRefs=False / InvalidRouteKinds listener condition).
func ListenerHasInvalidRouteKinds(ln gatewayv1.Listener) bool {
	if ln.AllowedRoutes == nil || len(ln.AllowedRoutes.Kinds) == 0 {
		return false
	}
	defaults := SupportedKindsFor(ln.Protocol)
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

// SupportedKindsFor maps a listener protocol to the Route kinds the edge serves
// on it: HTTPRoute for HTTP/HTTPS, TCPRoute for TCP, TLSRoute for TLS.
func SupportedKindsFor(protocol gatewayv1.ProtocolType) []gatewayv1.RouteGroupKind {
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
func (r *Resolver) namespaceAllowed(ctx context.Context, gwNamespace, routeNamespace string, ln gatewayv1.Listener) bool {
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
func (r *Resolver) httpRouteAttachment(
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
		if !parentRefNamesAnyListener(p, gw.Spec.Listeners) {
			continue
		}
		for _, ln := range gw.Spec.Listeners {
			lnReason, accepted := r.checkListenerAttach(ctx, gw, p, ln, routeNamespace, routeHostnames, reason)
			if accepted {
				return attachAccepted
			}
			reason = lnReason
		}
	}
	return reason
}

// checkListenerAttach tests whether parentRef p attaches to listener ln on Gateway gw
// for an HTTPRoute in routeNamespace with routeHostnames. Returns the updated reason
// and whether the listener fully accepted the route.
func (r *Resolver) checkListenerAttach(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	p gatewayv1.ParentReference,
	ln gatewayv1.Listener,
	routeNamespace string,
	routeHostnames []gatewayv1.Hostname,
	reason routeAttachReason,
) (routeAttachReason, bool) {
	if !parentRefSelectsListener(p, ln) {
		return reason, false
	}
	if !listenerAcceptsKind(ln, "HTTPRoute") {
		return reason, false
	}
	if !r.namespaceAllowed(ctx, gw.Namespace, routeNamespace, ln) {
		if reason == attachNoMatchingParent {
			reason = attachNotAllowedByListeners
		}
		return reason, false
	}
	if !hostnamesIntersect(ln.Hostname, routeHostnames) {
		return attachNoMatchingListenerHostname, false
	}
	return reason, true
}

// HTTPRouteAcceptance computes the route-level Accepted condition for an HTTPRoute
// across all of our Gateways it references. The route is Accepted when it attaches
// to at least one listener of at least one of our Gateways; otherwise the most
// specific failure reason wins (NoMatchingListenerHostname > NotAllowedByListeners
// > NoMatchingParent).
func (r *Resolver) HTTPRouteAcceptance(ctx context.Context, hr *gatewayv1.HTTPRoute, gwByKey map[GatewayKey]*gatewayv1.Gateway) (bool, string, string) {
	best := attachNoMatchingParent
	for _, p := range hr.Spec.ParentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := GatewayKey{Namespace: parentRefNamespace(p.Namespace, hr.Namespace), Name: string(p.Name)}
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

// AttachedRoutesForListener counts the routes (with Accepted=True) attached to a
// specific listener. HTTP/HTTPS listeners accept HTTPRoutes attached to the
// gateway; TCP/TLS listeners accept TCP/TLSRoutes whose parentRef resolves to
// that listener's port (via GatewayParentPorts).
func (r *Resolver) AttachedRoutesForListener(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	ln gatewayv1.Listener,
	httpRoutes []gatewayv1.HTTPRoute,
	tcpRoutes []gatewayv1.TCPRoute,
	tlsRoutes []gatewayv1.TLSRoute,
	gateways map[GatewayKey]struct{},
	listenerKeys map[GatewayListenerKey]struct{},
) int32 {
	switch ln.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		return r.countAttachedHTTPRoutes(ctx, gw, ln, httpRoutes)
	case gatewayv1.TCPProtocolType:
		return countAttachedTCPRoutes(tcpRoutes, gateways, listenerKeys, ln)
	case gatewayv1.TLSProtocolType:
		return countAttachedTLSRoutes(tlsRoutes, gateways, listenerKeys, ln)
	}
	return 0
}

func (r *Resolver) countAttachedHTTPRoutes(ctx context.Context, gw *gatewayv1.Gateway, ln gatewayv1.Listener, httpRoutes []gatewayv1.HTTPRoute) int32 {
	var count int32
	for i := range httpRoutes {
		hr := &httpRoutes[i]
		if r.httpRouteAttachesToListener(ctx, gw, hr.Spec.ParentRefs, hr.Namespace, hr.Spec.Hostnames, ln) {
			count++
		}
	}
	return count
}

func countAttachedTCPRoutes(routes []gatewayv1.TCPRoute, gateways map[GatewayKey]struct{}, listenerKeys map[GatewayListenerKey]struct{}, ln gatewayv1.Listener) int32 {
	var count int32
	port := uint32(ln.Port)
	for i := range routes {
		tr := &routes[i]
		if containsPort(GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, listenerKeys), port) {
			count++
		}
	}
	return count
}

func countAttachedTLSRoutes(routes []gatewayv1.TLSRoute, gateways map[GatewayKey]struct{}, listenerKeys map[GatewayListenerKey]struct{}, ln gatewayv1.Listener) int32 {
	var count int32
	port := uint32(ln.Port)
	for i := range routes {
		tr := &routes[i]
		if containsPort(GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, listenerKeys), port) {
			count++
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
func (r *Resolver) httpRouteAttachesToListener(
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
