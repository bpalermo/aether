// Package gatewayapi contains the edge proxy's Gateway API controller: it watches
// Gateway API HTTPRoutes, TCPRoutes, and TLSRoutes attached to a Gateway of the
// aether GatewayClass and projects them into the edge data plane (proposal 018 —
// north-south). It is the edge's only routing API (the VirtualHost CRD was retired);
// routing is direct-to-pod mTLS.
package gatewayapi

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// RouteSink receives the projected virtual hosts, TLS certs, and L4 routes
// (the snapshot cache).
type RouteSink interface {
	SetVirtualHosts(vhosts []cache.VirtualHost)
	SetEdgeTLSSecrets(ctx context.Context, certs map[string]cache.EdgeTLSCert) error
	SetEdgeTCPRoutes(routes []proxy.EdgeL4TCPRoute)
	SetEdgeTLSRoutes(routes []proxy.EdgeL4TLSRoute)
}

// Reconciler watches Gateway API HTTPRoutes, TCPRoutes, and TLSRoutes (and the
// Gateways/Secrets they depend on) in a namespace and projects, on any change,
// the complete virtual-host and L4 route sets into the cache. Level-based: each
// reconcile re-lists, so adds/updates/deletes converge without delta tracking —
// the same model as the VirtualHost reconciler.
type Reconciler struct {
	client.Client

	// Sink receives the projected virtual hosts/certs and L4 routes (the snapshot cache).
	Sink RouteSink
	// Namespace is the namespace Gateways/HTTPRoutes/TCPRoutes/TLSRoutes live in.
	Namespace string
	// GatewayClassName is the GatewayClass whose Gateways this edge serves.
	GatewayClassName string
	// Secrets resolves Gateway listener TLS cert material; nil disables TLS.
	Secrets *secret.Registry
	// MeshDomain is the mesh DNS domain used to build TCP cluster names.
	MeshDomain string
	Log        *slog.Logger
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
		Watches(&gatewayv1.Gateway{}, resync).
		Watches(&gatewayv1alpha2.TCPRoute{}, resync).
		Watches(&gatewayv1.TLSRoute{}, resync).
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
	gateways, ourGateways, gatewayListeners, hostCerts, certs, err := r.resolveGateways(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	// --- HTTPRoutes ---
	httpRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRoutes, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	// Deterministic order so keep-first host-dedup in the cache is stable.
	slices.SortFunc(httpRoutes.Items, func(a, b gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Name, b.Name)
	})
	vhosts := make([]cache.VirtualHost, 0, len(httpRoutes.Items))
	for i := range httpRoutes.Items {
		hr := &httpRoutes.Items[i]
		if !attachedToOurGateway(hr.Spec.ParentRefs, gateways) {
			continue
		}
		vh := r.buildVirtualHost(hr, hostCerts)
		if len(vh.Hosts) == 0 || len(vh.Routes) == 0 {
			continue
		}
		vhosts = append(vhosts, vh)
	}

	// --- TCPRoutes ---
	tcpRouteList := &gatewayv1alpha2.TCPRouteList{}
	if err := r.List(ctx, tcpRouteList, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	tcpByPort := make(map[uint32][]proxy.L4Backend)
	for i := range tcpRouteList.Items {
		tr := &tcpRouteList.Items[i]
		ports := gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TCPProtocolType, gatewayListeners)
		for _, port := range ports {
			for _, rule := range tr.Spec.Rules {
				tcpByPort[port] = append(tcpByPort[port], r.buildL4Backends(rule.BackendRefs)...)
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

	// --- TLSRoutes ---
	tlsRouteList := &gatewayv1.TLSRouteList{}
	if err := r.List(ctx, tlsRouteList, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	tlsByPort := make(map[uint32][]proxy.L4ServiceRoute)
	for i := range tlsRouteList.Items {
		tr := &tlsRouteList.Items[i]
		ports := gatewayParentPorts(tr.Spec.ParentRefs, gateways, gatewayv1.TLSProtocolType, gatewayListeners)
		hostnames := make([]string, 0, len(tr.Spec.Hostnames))
		for _, h := range tr.Spec.Hostnames {
			hostnames = append(hostnames, string(h))
		}
		for _, port := range ports {
			for _, rule := range tr.Spec.Rules {
				tlsByPort[port] = append(tlsByPort[port], proxy.L4ServiceRoute{
					SNIHostnames: hostnames,
					Backends:     r.buildL4Backends(rule.BackendRefs),
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

	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
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
	)

	// Publish Gateway API status (the conformance on-ramp). Failures are logged,
	// not fatal — the data plane is already projected and the next reconcile
	// retries. The GatewayClass, Gateways, and routes we own all get conditions.
	r.writeGatewayClassStatus(ctx)
	r.writeGatewayStatuses(ctx, ourGateways, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners)
	r.writeRouteStatuses(ctx, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners)
	return reconcile.Result{}, nil
}

// gatewayListenerKey identifies a listener within a gateway by name+protocol+port.
// Used to scope TCPRoute/TLSRoute parentRef port matching.
type gatewayListenerKey struct {
	Gateway  string
	Port     uint32
	Protocol gatewayv1.ProtocolType
}

// resolveGateways lists the Gateways of our GatewayClass and resolves each TLS
// listener's cert. It returns the set of our Gateway names, the Gateway objects
// themselves (for status), a set of listener keys (for TCPRoute/TLSRoute port
// matching), a hostname→SDS-name map for HTTP cert selection, and the
// SDS-name→bytes map to push.
func (r *Reconciler) resolveGateways(ctx context.Context) (map[string]struct{}, []gatewayv1.Gateway, map[gatewayListenerKey]struct{}, map[string]string, map[string]cache.EdgeTLSCert, error) {
	list := &gatewayv1.GatewayList{}
	if err := r.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	gateways := map[string]struct{}{}
	var ourGateways []gatewayv1.Gateway
	listenerKeys := map[gatewayListenerKey]struct{}{}
	hostCerts := map[string]string{} // listener hostname ("" = catch-all) -> SDS name
	certs := map[string]cache.EdgeTLSCert{}
	for i := range list.Items {
		gw := &list.Items[i]
		if string(gw.Spec.GatewayClassName) != r.GatewayClassName {
			continue
		}
		gateways[gw.Name] = struct{}{}
		ourGateways = append(ourGateways, *gw)
		for _, ln := range gw.Spec.Listeners {
			listenerKeys[gatewayListenerKey{
				Gateway:  gw.Name,
				Port:     uint32(ln.Port),
				Protocol: ln.Protocol,
			}] = struct{}{}
		}
		if r.Secrets == nil {
			continue
		}
		for _, ln := range gw.Spec.Listeners {
			if ln.TLS == nil {
				continue
			}
			host := ""
			if ln.Hostname != nil {
				host = string(*ln.Hostname)
			}
			for _, ref := range ln.TLS.CertificateRefs {
				if ref.Kind != nil && string(*ref.Kind) != "Secret" {
					continue
				}
				name := string(ref.Name)
				sdsName, sErr := r.Secrets.SDSName(configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, name)
				if sErr != nil {
					r.Log.WarnContext(ctx, "gateway listener TLS provider unavailable", "gateway", gw.Name, "secret", name, "error", sErr.Error())
					continue
				}
				cert, cErr := r.Secrets.Resolve(ctx, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, name)
				if cErr != nil {
					r.Log.WarnContext(ctx, "gateway listener TLS cert unresolved", "gateway", gw.Name, "secret", name, "error", cErr.Error())
					continue
				}
				certs[sdsName] = cache.EdgeTLSCert{Cert: cert.Cert, Key: cert.Key}
				hostCerts[host] = sdsName
				break // one cert per listener
			}
		}
	}
	return gateways, ourGateways, listenerKeys, hostCerts, certs, nil
}

// buildVirtualHost maps one HTTPRoute to an edge VirtualHost: its hostnames become
// the vhost domains, each rule's path match + first backendRef becomes a Route, and
// the matching Gateway listener cert (by hostname, falling back to a catch-all
// listener) sets the downstream TLS.
func (r *Reconciler) buildVirtualHost(hr *gatewayv1.HTTPRoute, hostCerts map[string]string) cache.VirtualHost {
	vh := cache.VirtualHost{}
	for _, h := range hr.Spec.Hostnames {
		vh.Hosts = append(vh.Hosts, string(h))
	}
	for _, rule := range hr.Spec.Rules {
		backend := firstBackendService(rule.BackendRefs)
		if backend == "" {
			continue
		}
		var port uint32
		if p := firstBackendPort(rule.BackendRefs); p != 0 {
			port = p
		}
		if len(rule.Matches) == 0 {
			vh.Routes = append(vh.Routes, cache.Route{Prefix: "/", Service: backend, Port: port})
			continue
		}
		for _, m := range rule.Matches {
			route := cache.Route{Service: backend, Port: port}
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

// attachedToOurGateway reports whether the given parentRefs include a reference
// to one of our Gateways (same namespace; Gateway kind/group).
func attachedToOurGateway(parentRefs []gatewayv1.ParentReference, gateways map[string]struct{}) bool {
	for _, p := range parentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		if _, ok := gateways[string(p.Name)]; ok {
			return true
		}
	}
	return false
}

// gatewayParentPorts returns the distinct ports of our Gateway listeners that
// match the given protocol and are referenced by the given parentRefs. Used to
// scope TCPRoute/TLSRoute attachments to the exact Gateway listener ports.
func gatewayParentPorts(
	parentRefs []gatewayv1alpha2.ParentReference,
	gateways map[string]struct{},
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
		gwName := string(p.Name)
		if _, ok := gateways[gwName]; !ok {
			continue
		}
		// If a sectionName (listener name) is specified, match only that listener;
		// otherwise match all listeners of the given protocol on this gateway.
		if p.Port != nil {
			port := uint32(*p.Port)
			key := gatewayListenerKey{Gateway: gwName, Port: port, Protocol: protocol}
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
			if k.Gateway == gwName && k.Protocol == protocol {
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
// cluster names. Refs with a non-core group or non-Service kind are skipped.
func (r *Reconciler) buildL4Backends(refs []gatewayv1.BackendRef) []proxy.L4Backend {
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
			Cluster: proxy.TCPClusterName(name, r.MeshDomain),
			Weight:  weight,
		})
	}
	return backends
}

func firstBackendService(refs []gatewayv1.HTTPBackendRef) string {
	for _, b := range refs {
		if b.Group != nil && string(*b.Group) != "" {
			continue // only core Services in Phase 1
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		return string(b.Name)
	}
	return ""
}

func firstBackendPort(refs []gatewayv1.HTTPBackendRef) uint32 {
	for _, b := range refs {
		if b.Port != nil {
			return uint32(*b.Port)
		}
	}
	return 0
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
