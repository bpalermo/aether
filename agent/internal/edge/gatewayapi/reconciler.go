// Package gatewayapi contains the edge proxy's Gateway API controller: it watches
// Gateway API HTTPRoutes attached to a Gateway of the aether GatewayClass and
// projects them into the same edge data plane the VirtualHost reconciler feeds
// (proposal 018, Phase 1 — north-south). Routing is direct-to-pod mTLS exactly as
// with VirtualHost; only the API surface differs.
package gatewayapi

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// RouteSink receives the projected virtual hosts and TLS certs (the snapshot cache).
type RouteSink interface {
	SetVirtualHosts(vhosts []cache.VirtualHost)
	SetEdgeTLSSecrets(ctx context.Context, certs map[string]cache.EdgeTLSCert) error
}

// Reconciler watches Gateway API HTTPRoutes (and the Gateways/Secrets they depend
// on) in a namespace and projects, on any change, the complete virtual-host set
// into the cache. Level-based: each reconcile re-lists, so adds/updates/deletes
// converge without delta tracking — the same model as the VirtualHost reconciler.
type Reconciler struct {
	client.Client

	// Sink receives the projected virtual hosts/certs (the snapshot cache).
	Sink RouteSink
	// Namespace is the namespace Gateways/HTTPRoutes (and their TLS Secrets) live in.
	Namespace string
	// GatewayClassName is the GatewayClass whose Gateways this edge serves.
	GatewayClassName string
	// Secrets resolves Gateway listener TLS cert material; nil disables TLS.
	Secrets *secret.Registry
	Log     *slog.Logger
}

// SetupWithManager registers the reconciler to watch HTTPRoutes, Gateways and (when
// TLS is enabled) Secrets — any change re-projects the whole set.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gatewayapi")
	resync := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "resync"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.Gateway{}, resync).
		Named("gatewayapi")
	if r.Secrets != nil {
		b = b.Watches(&corev1.Secret{}, resync)
	}
	return b.Complete(r)
}

// Reconcile re-lists our Gateways and the HTTPRoutes attached to them, resolves
// each Gateway listener's TLS cert, and replaces the cache's virtual-host set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Our Gateways (those of our GatewayClass) and, per listener hostname, the SDS
	// cert name to present. listenerCerts also accumulates the cert bytes to push.
	gateways, hostCerts, certs, err := r.resolveGateways(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	routes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routes, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	// Deterministic order so keep-first host-dedup in the cache is stable.
	slices.SortFunc(routes.Items, func(a, b gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Name, b.Name)
	})

	vhosts := make([]cache.VirtualHost, 0, len(routes.Items))
	for i := range routes.Items {
		hr := &routes.Items[i]
		if !attachedToOurGateway(hr, gateways) {
			continue
		}
		vh := r.buildVirtualHost(hr, hostCerts)
		if len(vh.Hosts) == 0 || len(vh.Routes) == 0 {
			continue
		}
		vhosts = append(vhosts, vh)
	}

	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
	}
	r.Sink.SetVirtualHosts(vhosts)
	r.Log.DebugContext(ctx, "projected gateway-api routes", "httpRoutes", len(routes.Items), "virtualHosts", len(vhosts), "tlsCerts", len(certs))
	return reconcile.Result{}, nil
}

// resolveGateways lists the Gateways of our GatewayClass and resolves each TLS
// listener's cert. It returns the set of our Gateway names, a hostname→SDS-name map
// for cert selection, and the SDS-name→bytes map to push.
func (r *Reconciler) resolveGateways(ctx context.Context) (map[string]struct{}, map[string]string, map[string]cache.EdgeTLSCert, error) {
	list := &gatewayv1.GatewayList{}
	if err := r.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return nil, nil, nil, err
	}
	gateways := map[string]struct{}{}
	hostCerts := map[string]string{} // listener hostname ("" = catch-all) -> SDS name
	certs := map[string]cache.EdgeTLSCert{}
	for i := range list.Items {
		gw := &list.Items[i]
		if string(gw.Spec.GatewayClassName) != r.GatewayClassName {
			continue
		}
		gateways[gw.Name] = struct{}{}
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
	return gateways, hostCerts, certs, nil
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

// attachedToOurGateway reports whether the HTTPRoute has a parentRef to one of our
// Gateways (same namespace; Gateway kind/group).
func attachedToOurGateway(hr *gatewayv1.HTTPRoute, gateways map[string]struct{}) bool {
	for _, p := range hr.Spec.ParentRefs {
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
