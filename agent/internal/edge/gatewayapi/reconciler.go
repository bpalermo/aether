// Package gatewayapi contains the edge proxy's Gateway API controller: it watches
// Gateway API HTTPRoutes, TCPRoutes, and TLSRoutes attached to a Gateway of the
// aether GatewayClass and projects them into the edge data plane (proposal 018 —
// north-south). It is the edge's only routing API (the VirtualHost CRD was retired);
// routing is direct-to-pod mTLS.
package gatewayapi

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/referencegrant"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	constants "github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// RouteSink receives the projected virtual hosts, TLS certs, and L4 routes
// (the snapshot cache).
type RouteSink interface {
	SetVirtualHosts(vhosts []cache.VirtualHost)
	SetEdgeTLSSecrets(ctx context.Context, certs map[string]cache.EdgeTLSCert) error
	SetEdgeTCPRoutes(routes []proxy.EdgeL4TCPRoute)
	SetEdgeTLSRoutes(routes []proxy.EdgeL4TLSRoute)
	// SetEdgeHTTPRedirect controls whether the HTTP-port listener 301-redirects
	// to https (true) or serves routes directly (false). The reconciler sets this
	// on every reconcile based on whether any Gateway in the current set carries
	// the gateway.aether.io/http-redirect: "true" annotation.
	SetEdgeHTTPRedirect(enabled bool)
	// SetEdgeGateways replaces the per-Gateway routing set (proposal 021 Phase 2).
	// When non-empty, the snapshot emits per-Gateway listeners and route configs
	// (one per Gateway, bound on allocated internal ports) instead of the shared
	// Phase 1 listeners. When empty, falls back to Phase 1 behavior.
	SetEdgeGateways(gateways []cache.EdgeGatewayEntry)
}

// Reconciler watches Gateway API HTTPRoutes, TCPRoutes, and TLSRoutes (and the
// Gateways/Secrets they depend on) CLUSTER-WIDE and projects, on any change, the
// complete virtual-host and L4 route sets into the cache. Level-based: each
// reconcile re-lists, so adds/updates/deletes converge without delta tracking —
// the same model as the VirtualHost reconciler. It is namespace-agnostic: a
// Gateway of our class in any namespace is reconciled and gets status, which is
// what the upstream conformance suite (running in its own namespaces) requires.
type Reconciler struct {
	client.Client

	// APIReader is an uncached reader (mgr.GetAPIReader) used to fetch the
	// cluster-scoped GatewayClass for status (a cached Get on a cluster-scoped
	// resource can miss before its informer has synced and return NotFound).
	APIReader client.Reader

	// Sink receives the projected virtual hosts/certs and L4 routes (the snapshot cache).
	Sink RouteSink
	// Namespace is the edge's own namespace. It is used as the default namespace
	// for the TLS Secret provider's cert lookups and as the namespace of the edge
	// LoadBalancer Service (EdgeServiceName) from which the published Gateway
	// status.addresses is resolved; Gateways/Routes are listed and reconciled
	// CLUSTER-WIDE (namespace-agnostic), so this field does not scope the
	// route/Gateway lists.
	Namespace string
	// EdgeServiceName is the name of the edge's own LoadBalancer Service (in
	// Namespace). Its status.loadBalancer.ingress address is published as every
	// class-aether Gateway's status.addresses (proposal 021 Phase 1 — the shared
	// edge address), and is used as the base name for per-Gateway Services in Phase 2.
	// Empty disables address publication.
	EdgeServiceName string
	// GatewayClassName is the GatewayClass whose Gateways this edge serves.
	GatewayClassName string
	// Secrets resolves Gateway listener TLS cert material; nil disables TLS.
	Secrets *secret.Registry
	// MeshDomain is the mesh DNS domain used to build TCP cluster names.
	MeshDomain string
	// PerGatewayAddressing enables proposal 021 Phase 2: a per-Gateway LoadBalancer
	// Service + internal-port demux so each Gateway gets its own external IP. When
	// false (or when EdgeServiceName is empty), the reconciler falls back to Phase 1
	// (single shared edge LB IP for all Gateways).
	PerGatewayAddressing bool
	Log                  *slog.Logger
}

// SetupWithManager registers the reconciler to watch HTTPRoutes, TCPRoutes,
// TLSRoutes, Gateways and (when TLS is enabled) Secrets — any change
// re-projects the whole set.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gatewayapi")
	resync := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "resync"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		// GatewayClass: a spec change must re-publish status (observedGeneration
		// bump). We reconcile any GatewayClass bearing our controllerName.
		Watches(&gatewayv1.GatewayClass{}, resync).
		Watches(&gatewayv1.Gateway{}, resync).
		Watches(&gatewayv1alpha2.TCPRoute{}, resync).
		Watches(&gatewayv1.TLSRoute{}, resync).
		// ReferenceGrant (v1beta1): a grant change can flip a cross-namespace
		// backendRef between permitted and RefNotPermitted, so re-project everything.
		Watches(&gatewayv1beta1.ReferenceGrant{}, resync).
		// Watch Services in the edge namespace: when MetalLB assigns a LoadBalancer
		// IP to a per-Gateway Service, the Service status changes and we must
		// re-publish status.addresses on the corresponding Gateway.
		Watches(&corev1.Service{}, resync).
		Named("gatewayapi")
	if r.Secrets != nil {
		b = b.Watches(&corev1.Secret{}, resync)
	}
	return b.Complete(r)
}

// Reconcile re-lists our Gateways and the HTTPRoutes, TCPRoutes, and TLSRoutes
// attached to them, resolves each Gateway listener's TLS cert, and replaces the
// cache's virtual-host and L4 route sets.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Our Gateways (those of our GatewayClass) and, per listener hostname, the SDS
	// cert name to present. listenerCerts also accumulates the cert bytes to push.
	gateways, ourGateways, gatewayListeners, hostCerts, certs, tlsResults, err := r.resolveGateways(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs (drop +
	// RefNotPermitted). The edge cache is cluster-wide (post-#323).
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := r.List(ctx, grantList); err != nil {
		return reconcile.Result{}, err
	}
	grants := grantList.Items

	// --- HTTPRoutes (cluster-wide) ---
	httpRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRoutes); err != nil {
		return reconcile.Result{}, err
	}
	// Deterministic order so keep-first host-dedup in the cache is stable. Sort by
	// namespace+name now that routes may span namespaces.
	slices.SortFunc(httpRoutes.Items, func(a, b gatewayv1.HTTPRoute) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	vhosts := make([]cache.VirtualHost, 0, len(httpRoutes.Items))
	for i := range httpRoutes.Items {
		hr := &httpRoutes.Items[i]
		if !attachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, gateways) {
			continue
		}
		vh := r.buildVirtualHost(hr, hostCerts, grants)
		if len(vh.Hosts) == 0 || len(vh.Routes) == 0 {
			continue
		}
		vhosts = append(vhosts, vh)
	}

	// --- TCPRoutes (cluster-wide) ---
	tcpRouteList := &gatewayv1alpha2.TCPRouteList{}
	if err := r.List(ctx, tcpRouteList); err != nil {
		return reconcile.Result{}, err
	}
	tcpByPort := make(map[uint32][]proxy.L4Backend)
	for i := range tcpRouteList.Items {
		tr := &tcpRouteList.Items[i]
		ports := gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, gatewayListeners)
		for _, port := range ports {
			for _, rule := range tr.Spec.Rules {
				tcpByPort[port] = append(tcpByPort[port], r.buildL4Backends(rule.BackendRefs, tr.Namespace, "TCPRoute", grants)...)
			}
		}
	}
	tcpRoutes := make([]proxy.EdgeL4TCPRoute, 0, len(tcpByPort))
	for port, backends := range tcpByPort {
		tcpRoutes = append(tcpRoutes, proxy.EdgeL4TCPRoute{Port: port, Backends: backends})
	}
	slices.SortFunc(tcpRoutes, func(a, b proxy.EdgeL4TCPRoute) int {
		if a.Port < b.Port {
			return -1
		}
		if a.Port > b.Port {
			return 1
		}
		return 0
	})

	// --- TLSRoutes (cluster-wide) ---
	tlsRouteList := &gatewayv1.TLSRouteList{}
	if err := r.List(ctx, tlsRouteList); err != nil {
		return reconcile.Result{}, err
	}
	tlsByPort := make(map[uint32][]proxy.L4ServiceRoute)
	for i := range tlsRouteList.Items {
		tr := &tlsRouteList.Items[i]
		ports := gatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, gatewayListeners)
		hostnames := make([]string, 0, len(tr.Spec.Hostnames))
		for _, h := range tr.Spec.Hostnames {
			hostnames = append(hostnames, string(h))
		}
		for _, port := range ports {
			for _, rule := range tr.Spec.Rules {
				tlsByPort[port] = append(tlsByPort[port], proxy.L4ServiceRoute{
					SNIHostnames: hostnames,
					Backends:     r.buildL4Backends(rule.BackendRefs, tr.Namespace, "TLSRoute", grants),
				})
			}
		}
	}
	tlsRoutes := make([]proxy.EdgeL4TLSRoute, 0, len(tlsByPort))
	for port, rules := range tlsByPort {
		tlsRoutes = append(tlsRoutes, proxy.EdgeL4TLSRoute{Port: port, Rules: rules})
	}
	slices.SortFunc(tlsRoutes, func(a, b proxy.EdgeL4TLSRoute) int {
		if a.Port < b.Port {
			return -1
		}
		if a.Port > b.Port {
			return 1
		}
		return 0
	})

	// Determine per-Gateway HTTP redirect opt-in (gateway.aether.io/http-redirect).
	// In Phase 1 this is a single bool; in Phase 2 it is tracked per Gateway key.
	httpRedirect := false
	gatewayHTTPRedirect := make(map[gatewayKey]bool, len(ourGateways))
	for i := range ourGateways {
		if ourGateways[i].Annotations[constants.AnnotationGatewayHTTPRedirect] == "true" {
			httpRedirect = true
			gk := gatewayKey{Namespace: ourGateways[i].Namespace, Name: ourGateways[i].Name}
			gatewayHTTPRedirect[gk] = true
		}
	}
	r.Sink.SetEdgeHTTPRedirect(httpRedirect)

	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Phase 2: per-Gateway LoadBalancer Services + per-Gateway listeners.
	// When PerGatewayAddressing is enabled and EdgeServiceName is set, we manage
	// per-Gateway Services and emit per-Gateway listeners. Otherwise fall back to
	// Phase 1 (shared SetVirtualHosts path).
	var perGWAssignedIPs map[gatewayKey]string
	if r.PerGatewayAddressing && r.EdgeServiceName != "" && len(ourGateways) > 0 {
		allocations, allocErr := allocateGatewayListenerPorts(ourGateways, hostCerts)
		if allocErr != nil {
			r.Log.WarnContext(ctx, "per-Gateway port allocation failed, falling back to Phase 1",
				"error", allocErr.Error())
			// Fall through to Phase 1 path.
			r.Sink.SetEdgeGateways(nil)
		} else {
			ips, svcErr := r.reconcileGatewayServices(ctx, ourGateways, allocations)
			if svcErr != nil {
				r.Log.WarnContext(ctx, "per-Gateway Service reconcile error", "error", svcErr.Error())
			}
			perGWAssignedIPs = ips
			entries := buildEdgeGatewayEntries(ourGateways, vhosts, allocations, gatewayHTTPRedirect)
			r.Sink.SetEdgeGateways(entries)
			r.Log.DebugContext(ctx, "projected per-Gateway entries",
				"gateways", len(entries))
			// In Phase 2 we still call SetVirtualHosts with the full vhosts so
			// the Phase 1 fallback route table is up-to-date (no-op at snapshot
			// time when per-Gateway mode is active, but keeps state consistent
			// for edge cases like a reconcile that loses the Phase 2 allocations).
		}
	} else {
		// Phase 1 / disabled: clear any stale Phase 2 state.
		r.Sink.SetEdgeGateways(nil)
	}
	r.Sink.SetVirtualHosts(vhosts)
	r.Sink.SetEdgeTCPRoutes(tcpRoutes)
	r.Sink.SetEdgeTLSRoutes(tlsRoutes)
	r.Log.DebugContext(ctx, "projected gateway-api routes",
		"httpRoutes", len(httpRoutes.Items),
		"tcpRoutes", len(tcpRouteList.Items),
		"tlsRoutes", len(tlsRouteList.Items),
		"virtualHosts", len(vhosts),
		"tcpListeners", len(tcpRoutes),
		"tlsListeners", len(tlsRoutes),
		"tlsCerts", len(certs),
		"perGatewayAddressing", r.PerGatewayAddressing,
	)

	// Publish Gateway API status (the conformance on-ramp). Failures are logged,
	// not fatal — the data plane is already projected and the next reconcile
	// retries. The GatewayClass, Gateways, and routes we own all get conditions.
	r.writeGatewayClassStatus(ctx)
	r.writeGatewayStatuses(ctx, ourGateways, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners, tlsResults, perGWAssignedIPs)
	r.writeRouteStatuses(ctx, ourGateways, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners, grants)
	return reconcile.Result{}, nil
}

// gatewayKey identifies a Gateway by namespace+name. Since the edge now
// reconciles cluster-wide, Gateway names alone are no longer unique (two
// namespaces may each have a Gateway "edge"), so route attachment must match on
// the full namespaced name.
type gatewayKey struct {
	Namespace string
	Name      string
}

// gatewayListenerKey identifies a listener within a gateway by
// namespace+name+protocol+port. Used to scope TCPRoute/TLSRoute parentRef port
// matching.
type gatewayListenerKey struct {
	Gateway  gatewayKey
	Port     uint32
	Protocol gatewayv1.ProtocolType
}

// listenerStatusKey identifies a listener within a Gateway by namespaced Gateway
// name + listener (section) name. Used to carry per-listener TLS-resolution
// results from resolveGateways into status writing (the InvalidCertificateRef
// gate).
type listenerStatusKey struct {
	Gateway gatewayKey
	Name    gatewayv1.SectionName
}

// listenerTLSResult records whether a TLS listener's certificateRefs resolved.
// hasTLS is true for any HTTPS/TLS listener that declares a tls block; resolved
// is true only when every (supported) certificateRef resolved to usable material.
// message carries the first failure for the listener condition.
type listenerTLSResult struct {
	hasTLS   bool
	resolved bool
	message  string
}

// resolveGateways lists the Gateways of our GatewayClass CLUSTER-WIDE and resolves
// each TLS listener's cert. It returns the set of our Gateways (by namespaced
// name), the Gateway objects themselves (for status), a set of listener keys (for
// TCPRoute/TLSRoute port matching), a hostname→SDS-name map for HTTP cert
// selection, and the SDS-name→bytes map to push. Selection is by
// GatewayClassName: every Gateway whose gatewayClassName is the class this edge
// serves (whose controllerName is gateway.aether.io/edge) is ours, regardless of
// namespace.
func (r *Reconciler) resolveGateways(ctx context.Context) (map[gatewayKey]struct{}, []gatewayv1.Gateway, map[gatewayListenerKey]struct{}, map[string]string, map[string]cache.EdgeTLSCert, map[listenerStatusKey]listenerTLSResult, error) {
	list := &gatewayv1.GatewayList{}
	if err := r.List(ctx, list); err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	gateways := map[gatewayKey]struct{}{}
	var ourGateways []gatewayv1.Gateway
	listenerKeys := map[gatewayListenerKey]struct{}{}
	hostCerts := map[string]string{} // listener hostname ("" = catch-all) -> SDS name
	certs := map[string]cache.EdgeTLSCert{}
	tlsResults := map[listenerStatusKey]listenerTLSResult{}
	for i := range list.Items {
		gw := &list.Items[i]
		if string(gw.Spec.GatewayClassName) != r.GatewayClassName {
			continue
		}
		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}
		gateways[gk] = struct{}{}
		ourGateways = append(ourGateways, *gw)
		for _, ln := range gw.Spec.Listeners {
			listenerKeys[gatewayListenerKey{
				Gateway:  gk,
				Port:     uint32(ln.Port),
				Protocol: ln.Protocol,
			}] = struct{}{}
		}
		for _, ln := range gw.Spec.Listeners {
			if ln.TLS == nil {
				continue
			}
			lk := listenerStatusKey{Gateway: gk, Name: ln.Name}
			res := r.resolveListenerTLS(ctx, gw, ln, hostCerts, certs)
			tlsResults[lk] = res
		}
	}
	return gateways, ourGateways, listenerKeys, hostCerts, certs, tlsResults, nil
}

// resolveListenerTLS resolves a single TLS listener's certificateRefs, recording
// each resolved cert into certs/hostCerts and returning whether the listener's TLS
// configuration is valid. A listener is INVALID (resolved=false) when any
// certificateRef has an unsupported group/kind, names a missing Secret, or holds
// malformed cert material — driving the ResolvedRefs=False/InvalidCertificateRef
// listener condition. With no Secrets provider configured, TLS cannot be resolved,
// so a TLS listener is reported invalid.
func (r *Reconciler) resolveListenerTLS(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	ln gatewayv1.Listener,
	hostCerts map[string]string,
	certs map[string]cache.EdgeTLSCert,
) listenerTLSResult {
	if len(ln.TLS.CertificateRefs) == 0 {
		return listenerTLSResult{hasTLS: true, resolved: false, message: "TLS listener has no certificateRefs"}
	}
	if r.Secrets == nil {
		return listenerTLSResult{hasTLS: true, resolved: false, message: "no TLS secret provider configured"}
	}
	host := ""
	if ln.Hostname != nil {
		host = string(*ln.Hostname)
	}
	var firstResolved string
	for _, ref := range ln.TLS.CertificateRefs {
		// Only core ("") group Secret kind is supported; any other group/kind is an
		// invalid certificateRef.
		if ref.Group != nil && string(*ref.Group) != "" {
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported group %q", ref.Name, *ref.Group)}
		}
		if ref.Kind != nil && string(*ref.Kind) != "Secret" {
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported kind %q", ref.Name, *ref.Kind)}
		}
		name := string(ref.Name)
		sdsName, sErr := r.Secrets.SDSName(configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, name)
		if sErr != nil {
			r.Log.WarnContext(ctx, "gateway listener TLS provider unavailable", "gateway", gw.Name, "secret", name, "error", sErr.Error())
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q provider unavailable", name)}
		}
		cert, cErr := r.Secrets.Resolve(ctx, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, name)
		if cErr != nil {
			r.Log.WarnContext(ctx, "gateway listener TLS cert unresolved", "gateway", gw.Name, "secret", name, "error", cErr.Error())
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q did not resolve: %v", name, cErr)}
		}
		certs[sdsName] = cache.EdgeTLSCert{Cert: cert.Cert, Key: cert.Key}
		if firstResolved == "" {
			firstResolved = sdsName
		}
	}
	if firstResolved != "" {
		hostCerts[host] = firstResolved
	}
	return listenerTLSResult{hasTLS: true, resolved: true}
}

// buildVirtualHost maps one HTTPRoute to an edge VirtualHost: its hostnames become
// the vhost domains, each rule's path match + first backendRef becomes a Route, and
// the matching Gateway listener cert (by hostname, falling back to a catch-all
// listener) sets the downstream TLS.
//
// Rules with a RequestRedirect filter emit a redirect route (no backend required).
// Rules with a URLRewrite filter apply host/path rewriting on the forwarding route.
func (r *Reconciler) buildVirtualHost(hr *gatewayv1.HTTPRoute, hostCerts map[string]string, grants []gatewayv1beta1.ReferenceGrant) cache.VirtualHost {
	vh := cache.VirtualHost{}
	for _, h := range hr.Spec.Hostnames {
		vh.Hosts = append(vh.Hosts, string(h))
	}
	for _, rule := range hr.Spec.Rules {
		redirect := buildHTTPRedirect(rule.Filters)
		urlRewrite := buildHTTPURLRewrite(rule.Filters)

		backend := firstBackendService(rule.BackendRefs, hr.Namespace, grants)
		// A rule must have either a backend or a redirect to produce a route.
		if backend == "" && redirect == nil {
			continue
		}
		var port uint32
		if p := firstBackendPort(rule.BackendRefs, hr.Namespace, grants); p != 0 {
			port = p
		}
		mutation := buildEdgeHTTPHeaderMutation(rule.Filters)
		if len(rule.Matches) == 0 {
			vh.Routes = append(vh.Routes, cache.Route{
				Prefix:         "/",
				Service:        backend,
				Port:           port,
				HeaderMutation: mutation,
				Redirect:       redirect,
				URLRewrite:     urlRewrite,
			})
			continue
		}
		for _, m := range rule.Matches {
			route := cache.Route{
				Service:        backend,
				Port:           port,
				HeaderMutation: mutation,
				Redirect:       redirect,
				URLRewrite:     urlRewrite,
			}
			if m.Path != nil && m.Path.Value != nil {
				switch ptrType(m.Path.Type, gatewayv1.PathMatchPathPrefix) {
				case gatewayv1.PathMatchExact:
					route.Exact = *m.Path.Value
				default: // PathPrefix (and unimplemented types) → prefix
					route.Prefix = *m.Path.Value
				}
			} else {
				route.Prefix = "/"
			}
			vh.Routes = append(vh.Routes, route)
		}
	}
	if cert := certForHosts(vh.Hosts, hostCerts); cert != "" {
		vh.TLSSecret = cert
	}
	return vh
}

// attachedToOurGateway reports whether the given parentRefs (belonging to a route
// in routeNamespace) include a reference to one of our Gateways. A parentRef's
// namespace defaults to the route's own namespace when unset (per the Gateway API
// spec), so cross-namespace attachment is matched correctly.
func attachedToOurGateway(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[gatewayKey]struct{}) bool {
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := gatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; ok {
			return true
		}
	}
	return false
}

// parentRefIsGateway reports whether a parentRef targets a Gateway (the default
// group/kind, or explicitly the Gateway API Gateway kind).
func parentRefIsGateway(p gatewayv1.ParentReference) bool {
	if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
		return false
	}
	if p.Kind != nil && string(*p.Kind) != "Gateway" {
		return false
	}
	return true
}

// parentRefNamespace resolves a parentRef namespace, defaulting to the referring
// route's namespace when the ref leaves it unset (Gateway API local-default rule).
func parentRefNamespace(ns *gatewayv1.Namespace, routeNamespace string) string {
	if ns != nil && *ns != "" {
		return string(*ns)
	}
	return routeNamespace
}

// gatewayParentPorts returns the distinct ports of our Gateway listeners that
// match the given protocol and are referenced by the given parentRefs (belonging
// to a route in routeNamespace). Used to scope TCPRoute/TLSRoute attachments to
// the exact Gateway listener ports. A parentRef namespace defaults to the route's
// own namespace when unset.
func gatewayParentPorts(
	parentRefs []gatewayv1alpha2.ParentReference,
	routeNamespace string,
	gateways map[gatewayKey]struct{},
	protocol gatewayv1.ProtocolType,
	listenerKeys map[gatewayListenerKey]struct{},
) []uint32 {
	seen := map[uint32]struct{}{}
	var ports []uint32
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		gk := gatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[gk]; !ok {
			continue
		}
		// If a sectionName (listener name) is specified, match only that listener;
		// otherwise match all listeners of the given protocol on this gateway.
		if p.Port != nil {
			port := uint32(*p.Port)
			key := gatewayListenerKey{Gateway: gk, Port: port, Protocol: protocol}
			if _, ok := listenerKeys[key]; ok {
				if _, dup := seen[port]; !dup {
					seen[port] = struct{}{}
					ports = append(ports, port)
				}
			}
			continue
		}
		// No port specified: include all matching-protocol listeners on this gateway.
		for k := range listenerKeys {
			if k.Gateway == gk && k.Protocol == protocol {
				if _, dup := seen[k.Port]; !dup {
					seen[k.Port] = struct{}{}
					ports = append(ports, k.Port)
				}
			}
		}
	}
	return ports
}

// buildL4Backends converts a BackendRef slice into L4Backends with resolved TCP
// cluster names. Refs with a non-core group or non-Service kind are skipped, as are
// ungranted cross-namespace refs (RefNotPermitted: dropped from the data plane).
// routeKind is the referring route's kind (TCPRoute/TLSRoute).
func (r *Reconciler) buildL4Backends(refs []gatewayv1.BackendRef, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) []proxy.L4Backend {
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
		backends = append(backends, proxy.L4Backend{
			Service: name,
			Cluster: proxy.TCPClusterName(name, r.MeshDomain),
			Weight:  weight,
		})
	}
	return backends
}

// firstBackendService returns the name of the first core-Service backendRef that is
// admissible: a same-namespace ref, or a cross-namespace ref permitted by a
// ReferenceGrant. Ungranted cross-namespace refs are skipped (RefNotPermitted →
// dropped from the data plane), so a rule whose only backend is ungranted yields no
// backend (and, with no redirect, no route).
func firstBackendService(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) string {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue // only core Services in Phase 1
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		if !backendPermitted(b.Namespace, routeNamespace, "HTTPRoute", string(b.Name), grants) {
			continue
		}
		return string(b.Name)
	}
	return ""
}

// firstBackendPort returns the port of the first admissible (same-namespace or
// granted cross-namespace) backendRef, so the port matches the backend
// firstBackendService selects.
func firstBackendPort(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) uint32 {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		if !backendPermitted(b.Namespace, routeNamespace, "HTTPRoute", string(b.Name), grants) {
			continue
		}
		if b.Port != nil {
			return uint32(*b.Port)
		}
	}
	return 0
}

// derefBackendNamespace returns the backendRef namespace ("" when unset).
func derefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// backendPermitted reports whether a backendRef is allowed onto the data plane: a
// same-namespace ref always is; a cross-namespace ref needs a matching ReferenceGrant
// in the backend's namespace whose from matches the route and whose to allows the
// Service. routeKind is the referring route's kind (HTTPRoute/TCPRoute/TLSRoute).
func backendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := derefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}

// certForHosts picks the SDS cert for a vhost: an exact hostname listener match
// first, then a wildcard listener (*.suffix), then a catch-all listener ("").
func certForHosts(hosts []string, hostCerts map[string]string) string {
	for _, h := range hosts {
		if c, ok := hostCerts[h]; ok {
			return c
		}
	}
	for _, h := range hosts {
		for lh, c := range hostCerts {
			if strings.HasPrefix(lh, "*.") && strings.HasSuffix(h, lh[1:]) {
				return c
			}
		}
	}
	if c, ok := hostCerts[""]; ok {
		return c
	}
	return ""
}

func ptrType(p *gatewayv1.PathMatchType, def gatewayv1.PathMatchType) gatewayv1.PathMatchType {
	if p == nil {
		return def
	}
	return *p
}

// buildHTTPRedirect extracts the first RequestRedirect filter and converts it to a
// GammaRedirect. Returns nil when no redirect filter is present.
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

// buildHTTPURLRewrite extracts the first URLRewrite filter and converts it to a
// GammaURLRewrite. Returns nil when no URLRewrite filter is present.
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

// buildEdgeHTTPHeaderMutation merges all RequestHeaderModifier and
// ResponseHeaderModifier filters in an edge HTTPRoute rule's filter list into a
// single GammaHeaderMutation. Unknown filter types (redirect, rewrite, mirror)
// are silently skipped. Returns nil when no modifier filter is present.
func buildEdgeHTTPHeaderMutation(filters []gatewayv1.HTTPRouteFilter) *proxy.GammaHeaderMutation {
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
			// Redirect, rewrite, mirror: handled by dedicated builders above; skip here.
		}
	}
	return m
}
