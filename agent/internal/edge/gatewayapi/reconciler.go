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
	"time"

	"github.com/bpalermo/aether/common/serviceref"

	"github.com/bpalermo/aether/agent/internal/edge/gatewayapi/attachment"
	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/referencegrant"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// AnnotationGatewayHTTPRedirect is a Gateway annotation that opts a plain-HTTP
// listener into HTTP→HTTPS redirect behaviour. When set to "true" on a Gateway
// of the aether GatewayClass, the edge emits a 301-redirect listener on the
// Gateway's HTTP port instead of serving routes directly.
//
// Default absent / "false" → the HTTP listener serves its attached HTTPRoutes
// (no redirect). Operators MUST set this annotation on any Gateway whose HTTP
// listener should redirect to HTTPS (e.g. the production api.palermo.dev edge
// Gateway). The aether chart sets it automatically when edge.tls.enabled is true.
const AnnotationGatewayHTTPRedirect = "gateway.aether.io/http-redirect"

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
	// HasRegistryService reports whether the named service is currently known to
	// the mesh registry (namespace-blind, bare service name). Used by the
	// backend-existence check to distinguish mesh/registry services (which need
	// no k8s Service object in the route namespace) from k8s-only Services.
	// Returns false when the registry does not implement ServiceCatalog or no
	// registry has been configured (safe default = not a registry service).
	HasRegistryService(name string) bool
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
	Log        *slog.Logger
}

// SetupWithManager registers the reconciler to watch HTTPRoutes, TCPRoutes,
// TLSRoutes, Gateways and (when TLS is enabled) Secrets — any change
// re-projects the whole set.
//
// The reconciler runs on EVERY edge replica (NeedLeaderElection=false). The
// edge is a multi-replica Deployment; each replica hosts its own xDS server
// that serves a co-located Envoy. Per-Gateway Envoy listeners are injected
// into the replica's own SnapshotCache via SetEdgeGateways — a call that only
// the leader's reconciler would make if leader-only. The follower's Envoy
// would then see no listeners on the allocated internal ports, causing
// "connection refused" for ~50% of connections (those kube-proxy routes to the
// follower pod). Running the reconciler on all replicas ensures each pod's
// SnapshotCache is updated and its co-located Envoy receives the correct
// per-Gateway listener set.
//
// K8s resource writes (per-Gateway Service create/update, Gateway status
// patches) are safe under concurrent reconciliation: the CreateOrUpdate pattern
// is idempotent, and status patches use server-side apply / optimistic
// concurrency (the controller-runtime client retries on 409 Conflict).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gatewayapi")
	resync := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "resync"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).
		// Run on every replica, not just the leader: each edge pod's co-located
		// Envoy must receive its own SnapshotCache update (per-Gateway listeners).
		// Without this, the follower replica's Envoy has no listener on the
		// internal ports and any connection kube-proxy routes to it is refused.
		WithOptions(controller.Options{NeedLeaderElection: boolPtr(false)}).
		For(&gatewayv1.HTTPRoute{}).
		// GatewayClass: a spec change must re-publish status (observedGeneration
		// bump). We reconcile any GatewayClass bearing our controllerName.
		Watches(&gatewayv1.GatewayClass{}, resync).
		Watches(&configapisv1.EdgeConfig{}, resync).
		Watches(&gatewayv1.Gateway{}, resync).
		Watches(&gatewayv1.TCPRoute{}, resync).
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
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := r.List(ctx, grantList); err != nil {
		return reconcile.Result{}, err
	}
	grants := grantList.Items

	gateways, ourGateways, gatewayListeners, perGWHostCerts, hostCerts, certs, tlsResults, err := r.resolveGateways(ctx, grants)
	if err != nil {
		return reconcile.Result{}, err
	}

	gwListenerHostnames := attachment.BuildGatewayListenerHostnames(ourGateways)

	httpRoutes, vhosts, err := r.listAndProjectHTTPRoutes(ctx, gateways, gwListenerHostnames, hostCerts, grants)
	if err != nil {
		return reconcile.Result{}, err
	}

	tcpRouteList, tcpRoutes, err := r.listAndProjectTCPRoutes(ctx, gateways, gatewayListeners, grants)
	if err != nil {
		return reconcile.Result{}, err
	}

	tlsRouteList, tlsRoutes, err := r.listAndProjectTLSRoutes(ctx, gateways, gatewayListeners, grants)
	if err != nil {
		return reconcile.Result{}, err
	}

	httpRedirect, gatewayHTTPRedirect := computeHTTPRedirect(ourGateways)
	r.Sink.SetEdgeHTTPRedirect(httpRedirect)

	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
	}

	perGWAssignedIPs := r.reconcilePhase2(ctx, ourGateways, perGWHostCerts, vhosts, gatewayHTTPRedirect)

	r.Sink.SetVirtualHosts(vhosts)
	r.Sink.SetEdgeTCPRoutes(tcpRoutes)
	r.Sink.SetEdgeTLSRoutes(tlsRoutes)
	r.Log.DebugContext(
		ctx, "projected gateway-api routes",
		"httpRoutes", len(httpRoutes.Items),
		"tcpRoutes", len(tcpRouteList.Items),
		"tlsRoutes", len(tlsRouteList.Items),
		"virtualHosts", len(vhosts),
		"tcpListeners", len(tcpRoutes),
		"tlsListeners", len(tlsRoutes),
		"tlsCerts", len(certs),
	)

	r.writeGatewayClassStatus(ctx)
	r.writeGatewayStatuses(ctx, ourGateways, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners, tlsResults, perGWAssignedIPs)
	r.writeRouteStatuses(ctx, ourGateways, httpRoutes.Items, tcpRouteList.Items, tlsRouteList.Items, gateways, gatewayListeners, grants)
	return reconcile.Result{}, nil
}

// listAndProjectHTTPRoutes lists all HTTPRoutes and builds virtual hosts for those
// attached to our Gateways. Returns the raw list (for status) and the projected vhosts.
func (r *Reconciler) listAndProjectHTTPRoutes(
	ctx context.Context,
	gateways map[gatewayKey]struct{},
	gwListenerHostnames map[string][]string,
	hostCerts map[string]string,
	grants []gatewayv1beta1.ReferenceGrant,
) (*gatewayv1.HTTPRouteList, []cache.VirtualHost, error) {
	httpRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRoutes); err != nil {
		return nil, nil, err
	}
	// Deterministic order: oldest creationTimestamp first, then namespace, then name.
	slices.SortFunc(httpRoutes.Items, func(a, b gatewayv1.HTTPRoute) int {
		ta := a.CreationTimestamp.Time
		tb := b.CreationTimestamp.Time
		if ta.Before(tb) {
			return -1
		}
		if tb.Before(ta) {
			return 1
		}
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	vhosts := make([]cache.VirtualHost, 0, len(httpRoutes.Items))
	for i := range httpRoutes.Items {
		hr := &httpRoutes.Items[i]
		if !attachment.AttachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, gateways) {
			continue
		}
		vh := r.buildVirtualHost(ctx, hr, hostCerts, grants)
		if len(vh.Routes) == 0 {
			continue
		}
		vh.Gateways = attachment.AttachedGatewayKeys(hr.Spec.ParentRefs, hr.Namespace, gateways)
		hostnameLookupKeys := attachment.AttachedHostnameLookupKeys(hr.Spec.ParentRefs, hr.Namespace, gateways)
		vh.Hosts = attachment.EffectiveHostnames(vh.Hosts, hostnameLookupKeys, gwListenerHostnames)
		if len(hr.Spec.Hostnames) > 0 && len(vh.Hosts) == 0 {
			continue
		}
		vhosts = append(vhosts, vh)
	}
	return httpRoutes, vhosts, nil
}

// listAndProjectTCPRoutes lists all TCPRoutes and projects them into per-port backends.
func (r *Reconciler) listAndProjectTCPRoutes(
	ctx context.Context,
	gateways map[gatewayKey]struct{},
	gatewayListeners map[gatewayListenerKey]struct{},
	grants []gatewayv1beta1.ReferenceGrant,
) (*gatewayv1.TCPRouteList, []proxy.EdgeL4TCPRoute, error) {
	tcpRouteList := &gatewayv1.TCPRouteList{}
	if err := r.List(ctx, tcpRouteList); err != nil {
		return nil, nil, err
	}
	tcpByPort := make(map[uint32][]proxy.L4Backend)
	for i := range tcpRouteList.Items {
		tr := &tcpRouteList.Items[i]
		ports := attachment.GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TCPProtocolType, gatewayListeners)
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
	return tcpRouteList, tcpRoutes, nil
}

// listAndProjectTLSRoutes lists all TLSRoutes and projects them into per-port service routes.
func (r *Reconciler) listAndProjectTLSRoutes(
	ctx context.Context,
	gateways map[gatewayKey]struct{},
	gatewayListeners map[gatewayListenerKey]struct{},
	grants []gatewayv1beta1.ReferenceGrant,
) (*gatewayv1.TLSRouteList, []proxy.EdgeL4TLSRoute, error) {
	tlsRouteList := &gatewayv1.TLSRouteList{}
	if err := r.List(ctx, tlsRouteList); err != nil {
		return nil, nil, err
	}
	tlsByPort := make(map[uint32][]proxy.L4ServiceRoute)
	for i := range tlsRouteList.Items {
		tr := &tlsRouteList.Items[i]
		ports := attachment.GatewayParentPorts(tr.Spec.ParentRefs, tr.Namespace, gateways, gatewayv1.TLSProtocolType, gatewayListeners)
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
	return tlsRouteList, tlsRoutes, nil
}

// computeHTTPRedirect scans ourGateways for the http-redirect annotation.
// Returns the Phase 1 bool and the per-Gateway map.
func computeHTTPRedirect(ourGateways []gatewayv1.Gateway) (bool, map[gatewayKey]bool) {
	httpRedirect := false
	gatewayHTTPRedirect := make(map[gatewayKey]bool, len(ourGateways))
	for i := range ourGateways {
		if ourGateways[i].Annotations[AnnotationGatewayHTTPRedirect] == "true" {
			httpRedirect = true
			gk := gatewayKey{Namespace: ourGateways[i].Namespace, Name: ourGateways[i].Name}
			gatewayHTTPRedirect[gk] = true
		}
	}
	return httpRedirect, gatewayHTTPRedirect
}

// reconcilePhase2 handles the per-Gateway Service / per-Gateway listener (Phase 2) path,
// or falls back to Phase 1 (SetEdgeGateways(nil)). Returns the assigned IPs map.
func (r *Reconciler) reconcilePhase2(
	ctx context.Context,
	ourGateways []gatewayv1.Gateway,
	perGWHostCerts map[gatewayKey]map[string]string,
	vhosts []cache.VirtualHost,
	gatewayHTTPRedirect map[gatewayKey]bool,
) map[gatewayKey]string {
	if r.EdgeServiceName == "" || len(ourGateways) == 0 {
		r.Sink.SetEdgeGateways(nil)
		if r.EdgeServiceName != "" {
			if err := r.gcStaleGatewayServices(ctx, map[string]struct{}{}); err != nil {
				r.Log.WarnContext(ctx, "GC of stale per-Gateway Services failed", "error", err.Error())
			}
		}
		return nil
	}

	allocations, allocErr := allocateGatewayListenerPorts(ourGateways, perGWHostCerts)
	if allocErr != nil {
		r.Log.WarnContext(ctx, "per-Gateway port allocation failed, falling back to Phase 1",
			"error", allocErr.Error())
		r.Sink.SetEdgeGateways(nil)
		return nil
	}

	edgeConfigs := make(map[gatewayKey]*configv1.EdgeConfigSpec, len(ourGateways))
	for i := range ourGateways {
		gw := &ourGateways[i]
		edgeConfigs[gatewayKey{Namespace: gw.Namespace, Name: gw.Name}] = r.resolveEdgeConfig(ctx, gw)
	}
	ips, svcErr := r.reconcileGatewayServices(ctx, ourGateways, allocations, edgeConfigs)
	if svcErr != nil {
		r.Log.WarnContext(ctx, "per-Gateway Service reconcile error", "error", svcErr.Error())
	}
	entries := buildEdgeGatewayEntries(ourGateways, vhosts, allocations, gatewayHTTPRedirect, edgeConfigs)
	r.Sink.SetEdgeGateways(entries)
	r.Log.DebugContext(ctx, "projected per-Gateway entries",
		"gateways", len(entries), "ourGateways", len(ourGateways),
		"totalVhosts", len(vhosts))
	return ips
}

// gatewayKey / gatewayListenerKey are the attachment package's Gateway and
// Gateway-listener identity types (route→listener attachment resolution lives
// there); aliased locally since they key most of this package's maps.
type (
	gatewayKey         = attachment.GatewayKey
	gatewayListenerKey = attachment.GatewayListenerKey
)

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
	// reason overrides the default ResolvedRefs=False reason
	// (InvalidCertificateRef) when set — e.g. RefNotPermitted for an ungranted
	// cross-namespace certificateRef.
	reason string
}

// resolveGateways lists the Gateways of our GatewayClass CLUSTER-WIDE and resolves
// each TLS listener's cert. It returns the set of our Gateways (by namespaced
// name), the Gateway objects themselves (for status), a set of listener keys (for
// TCPRoute/TLSRoute port matching), a per-Gateway hostname→SDS-name map (for
// per-Gateway cert selection in Phase 2), a merged hostname→SDS-name map (for
// Phase 1 / VirtualHost cert tagging), and the SDS-name→bytes map to push.
// Selection is by GatewayClassName: every Gateway whose gatewayClassName is the
// class this edge serves (whose controllerName is gateway.aether.io/edge) is ours,
// regardless of namespace.
//
// The per-Gateway hostCerts map is keyed by gatewayKey; each value is the
// hostname→SDS-name map for that Gateway's listeners only. This prevents cert
// cross-contamination when multiple Gateways share a catch-all TLS listener (empty
// hostname): the shared flat map would let a later-resolved Gateway overwrite the
// catch-all entry and cause an earlier Gateway's listeners to reference the wrong
// cert (presenting a cert for a different domain → TLS handshake failure).
func (r *Reconciler) resolveGateways(ctx context.Context, grants []gatewayv1beta1.ReferenceGrant) (map[gatewayKey]struct{}, []gatewayv1.Gateway, map[gatewayListenerKey]struct{}, map[gatewayKey]map[string]string, map[string]string, map[string]cache.EdgeTLSCert, map[listenerStatusKey]listenerTLSResult, error) {
	list := &gatewayv1.GatewayList{}
	if err := r.List(ctx, list); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	gateways := map[gatewayKey]struct{}{}
	var ourGateways []gatewayv1.Gateway
	listenerKeys := map[gatewayListenerKey]struct{}{}
	// perGWHostCerts is keyed per Gateway: each Gateway's TLS listeners populate only
	// their own entry. allocateGatewayListenerPorts uses this map so that cert
	// selection is scoped to the Gateway whose listener is being allocated — not
	// contaminated by another Gateway's certs that happen to share the same hostname
	// key (e.g., the catch-all "").
	perGWHostCerts := map[gatewayKey]map[string]string{}
	// mergedHostCerts is the flat union of all Gateways' hostCerts, used for Phase 1
	// VirtualHost cert tagging (buildVirtualHost) where the exact-gateway scope is
	// not required. Last-writer-wins on key collisions here is acceptable because
	// Phase 1 VirtualHost.TLSSecret is not used in Phase 2 listener selection.
	mergedHostCerts := map[string]string{}
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
		// Resolve TLS for each listener into a per-Gateway hostCerts map and the
		// global certs map (SDS secret bytes). Use a fresh per-Gateway hostCerts so
		// that resolveListenerTLS writes only into this Gateway's own scope.
		gwHostCerts := map[string]string{}
		for _, ln := range gw.Spec.Listeners {
			if ln.TLS == nil {
				continue
			}
			lk := listenerStatusKey{Gateway: gk, Name: ln.Name}
			res := r.resolveListenerTLS(ctx, gw, ln, gwHostCerts, certs, grants)
			tlsResults[lk] = res
		}
		perGWHostCerts[gk] = gwHostCerts
		// Merge into the flat map for Phase 1 / buildVirtualHost usage.
		for k, v := range gwHostCerts {
			mergedHostCerts[k] = v
		}
	}
	return gateways, ourGateways, listenerKeys, perGWHostCerts, mergedHostCerts, certs, tlsResults, nil
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
	grants []gatewayv1beta1.ReferenceGrant,
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
		sdsName, fail := r.resolveCertRef(ctx, gw, ref, certs, grants)
		if fail != nil {
			return *fail
		}
		if firstResolved == "" {
			firstResolved = sdsName
		}
	}
	if firstResolved != "" {
		hostCerts[host] = firstResolved
	}
	return listenerTLSResult{hasTLS: true, resolved: true}
}

// resolveCertRef validates and resolves a single certificateRef. Returns the SDS name
// on success, or a non-nil listenerTLSResult on failure.
func (r *Reconciler) resolveCertRef(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	ref gatewayv1.SecretObjectReference,
	certs map[string]cache.EdgeTLSCert,
	grants []gatewayv1beta1.ReferenceGrant,
) (string, *listenerTLSResult) {
	if ref.Group != nil && string(*ref.Group) != "" {
		return "", &listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported group %q", ref.Name, *ref.Group)}
	}
	if ref.Kind != nil && string(*ref.Kind) != "Secret" {
		return "", &listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported kind %q", ref.Name, *ref.Kind)}
	}
	name := string(ref.Name)
	certNS := gw.Namespace
	if ref.Namespace != nil && string(*ref.Namespace) != "" {
		certNS = string(*ref.Namespace)
	}
	if referencegrant.CrossNamespace(certNS, gw.Namespace) &&
		!referencegrant.PermitsSecret(grants, gw.Namespace, certNS, name) {
		fail := listenerTLSResult{hasTLS: true, resolved: false, reason: string(gatewayv1.ListenerReasonRefNotPermitted), message: fmt.Sprintf("certificateRef %s/%s is not permitted by any ReferenceGrant", certNS, name)}
		return "", &fail
	}
	secretRef := certNS + "/" + name
	sdsName, sErr := r.Secrets.SDSName(configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, secretRef)
	if sErr != nil {
		r.Log.WarnContext(ctx, "gateway listener TLS provider unavailable", "gateway", gw.Name, "secret", secretRef, "error", sErr.Error())
		return "", &listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q provider unavailable", name)}
	}
	cert, cErr := r.Secrets.Resolve(ctx, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, secretRef)
	if cErr != nil {
		r.Log.WarnContext(ctx, "gateway listener TLS cert unresolved", "gateway", gw.Name, "secret", secretRef, "error", cErr.Error())
		return "", &listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q did not resolve: %v", name, cErr)}
	}
	certs[sdsName] = cache.EdgeTLSCert{Cert: cert.Cert, Key: cert.Key}
	return sdsName, nil
}

// buildVirtualHost maps one HTTPRoute to an edge VirtualHost: its hostnames become
// the vhost domains, each rule's path match + backendRefs becomes a Route (with
// weighted backends when there are multiple), and the matching Gateway listener
// cert (by hostname, falling back to a catch-all listener) sets the downstream TLS.
//
// Rules with a RequestRedirect filter emit a redirect route (no backend required).
// Rules with a URLRewrite filter apply host/path rewriting on the forwarding route.
//
// Gateway API weight semantics: backendRef.weight defaults to 1 when nil. A rule
// where ALL backends are invalid/ungranted is skipped (no route). A rule where all
// valid backends have weight 0 is a valid route that produces no traffic (spec
// allows it; Envoy returns 500 for that case — acceptable).
//
// Gateway API invalid-backend semantics: a rule whose backendRef(s) are
// unresolvable (BackendNotFound / InvalidKind / RefNotPermitted — the conditions
// that set ResolvedRefs=False on the route status) must return HTTP 500 on the
// data plane rather than 503/404. The route still matches on path + headers +
// method + query (so the right rule is exercised) but emits a fixed
// direct_response 500 instead of routing to any cluster.
func (r *Reconciler) buildVirtualHost(ctx context.Context, hr *gatewayv1.HTTPRoute, hostCerts map[string]string, grants []gatewayv1beta1.ReferenceGrant) cache.VirtualHost {
	vh := cache.VirtualHost{}
	for _, h := range hr.Spec.Hostnames {
		vh.Hosts = append(vh.Hosts, string(h))
	}
	for _, rule := range hr.Spec.Rules {
		routes, skip := r.buildRoutesForRule(ctx, hr, rule, grants)
		if skip {
			continue
		}
		vh.Routes = append(vh.Routes, routes...)
	}
	if cert := certForHosts(vh.Hosts, hostCerts); cert != "" {
		vh.TLSSecret = cert
	}
	return vh
}

// buildRoutesForRule converts one HTTPRoute rule into cache.Routes. Returns (routes, skip=true)
// when the rule should be omitted entirely (no backends, no redirect, not an invalid-backend 500 case).
func (r *Reconciler) buildRoutesForRule(
	ctx context.Context,
	hr *gatewayv1.HTTPRoute,
	rule gatewayv1.HTTPRouteRule,
	grants []gatewayv1beta1.ReferenceGrant,
) ([]cache.Route, bool) {
	redirect := buildHTTPRedirect(rule.Filters)
	urlRewrite := buildHTTPURLRewrite(rule.Filters)
	timeout := buildRouteTimeout(rule.Timeouts)
	backends := r.buildHTTPRouteBackends(ctx, rule.BackendRefs, hr.Namespace, grants)

	if redirect == nil && len(backends) == 0 {
		return r.buildInvalidOrEmptyRoutes(ctx, hr, rule, grants)
	}

	mutation := buildEdgeHTTPHeaderMutation(rule.Filters)
	var legacySvc, legacyNS string
	var legacyPort, legacyDialPort uint32
	if len(backends) > 0 {
		legacySvc = backends[0].Service
		legacyPort = backends[0].Port
		legacyNS = backends[0].BackendNamespace
		legacyDialPort = backends[0].DialPort
	}

	if len(rule.Matches) == 0 {
		return []cache.Route{{
			Prefix:           "/",
			Service:          legacySvc,
			Port:             legacyPort,
			BackendNamespace: legacyNS,
			DialPort:         legacyDialPort,
			Backends:         backends,
			HeaderMutation:   mutation,
			Redirect:         redirect,
			URLRewrite:       urlRewrite,
			Timeout:          timeout,
		}}, false
	}

	routes := make([]cache.Route, 0, len(rule.Matches))
	for _, m := range rule.Matches {
		prefix, exact := buildMatchPath(m)
		routes = append(routes, cache.Route{
			Prefix:           prefix,
			Exact:            exact,
			Service:          legacySvc,
			Port:             legacyPort,
			BackendNamespace: legacyNS,
			DialPort:         legacyDialPort,
			Backends:         backends,
			HeaderMutation:   mutation,
			Redirect:         redirect,
			URLRewrite:       urlRewrite,
			Headers:          buildMatchHeaders(m),
			Method:           buildMatchMethod(m),
			QueryParams:      buildMatchQueryParams(m),
			Timeout:          timeout,
		})
	}
	return routes, false
}

// buildInvalidOrEmptyRoutes handles the no-backend / no-redirect case: emits 500 routes for
// unresolvable backends, or signals skip for truly inert rules.
func (r *Reconciler) buildInvalidOrEmptyRoutes(
	ctx context.Context,
	hr *gatewayv1.HTTPRoute,
	rule gatewayv1.HTTPRouteRule,
	grants []gatewayv1beta1.ReferenceGrant,
) ([]cache.Route, bool) {
	// No declared backendRefs → inert rule, skip.
	if len(rule.BackendRefs) == 0 {
		// gateway-api 1.6 HTTPRouteNoBackendRefs: empty list also emits 500.
		// Fall through to the unresolved path below.
	} else {
		resolved, _, _ := r.backendsResolveHTTP(ctx, hr.Namespace, []gatewayv1.HTTPRouteRule{rule}, grants)
		if resolved {
			// All backends resolved but none admissible — inert rule (no redirect, no backend).
			return nil, true
		}
	}
	// Unresolvable backends or empty list: emit 500 routes.
	mutation := buildEdgeHTTPHeaderMutation(rule.Filters)
	if len(rule.Matches) == 0 {
		return []cache.Route{{
			Prefix:               "/",
			HeaderMutation:       mutation,
			DirectResponseStatus: 500,
		}}, false
	}
	routes := make([]cache.Route, 0, len(rule.Matches))
	for _, m := range rule.Matches {
		prefix, exact := buildMatchPath(m)
		routes = append(routes, cache.Route{
			Prefix:               prefix,
			Exact:                exact,
			Headers:              buildMatchHeaders(m),
			Method:               buildMatchMethod(m),
			QueryParams:          buildMatchQueryParams(m),
			HeaderMutation:       mutation,
			DirectResponseStatus: 500,
		})
	}
	return routes, false
}

// buildMatchPath extracts the prefix/exact path values from an HTTPRouteMatch.
func buildMatchPath(m gatewayv1.HTTPRouteMatch) (prefix, exact string) {
	prefix = "/"
	if m.Path == nil || m.Path.Value == nil {
		return
	}
	switch ptrType(m.Path.Type, gatewayv1.PathMatchPathPrefix) {
	case gatewayv1.PathMatchExact:
		return "", *m.Path.Value
	default:
		return *m.Path.Value, ""
	}
}

// buildMatchHeaders converts HTTPRoute header match predicates to proxy types.
func buildMatchHeaders(m gatewayv1.HTTPRouteMatch) []proxy.RouteHeaderMatch {
	if len(m.Headers) == 0 {
		return nil
	}
	hdrs := make([]proxy.RouteHeaderMatch, 0, len(m.Headers))
	for _, h := range m.Headers {
		hm := proxy.RouteHeaderMatch{Name: string(h.Name), Value: h.Value}
		if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
			hm.Regex = true
		}
		hdrs = append(hdrs, hm)
	}
	return hdrs
}

// buildMatchMethod returns the method string from an HTTPRouteMatch (empty when unset).
func buildMatchMethod(m gatewayv1.HTTPRouteMatch) string {
	if m.Method == nil {
		return ""
	}
	return string(*m.Method)
}

// buildMatchQueryParams converts HTTPRoute query-parameter match predicates to proxy types.
func buildMatchQueryParams(m gatewayv1.HTTPRouteMatch) []proxy.RouteQueryParamMatch {
	if len(m.QueryParams) == 0 {
		return nil
	}
	qps := make([]proxy.RouteQueryParamMatch, 0, len(m.QueryParams))
	for _, q := range m.QueryParams {
		qm := proxy.RouteQueryParamMatch{Name: string(q.Name), Value: q.Value}
		if q.Type != nil && *q.Type == gatewayv1.QueryParamMatchRegularExpression {
			qm.Regex = true
		}
		qps = append(qps, qm)
	}
	return qps
}

// backendExists reports whether a named backend is admissible on the data plane:
// it exists if it is a mesh/registry service (resolved namespace-blind via the
// registry ServiceCatalog) OR if a k8s Service with the given name/namespace can
// be read from the API server. The registry is checked first (cheap, in-memory).
//
// Only a hard k8s NotFound with no registry hit causes the backend to be treated
// as non-existent. Transient API errors leave the backend admitted to avoid
// false-positive 500s on a momentary API server hiccup.
//
// When r.Sink is nil or r.Client is nil, the check degrades gracefully (the
// backend is admitted) to preserve prior behavior for callers that don't wire up
// those dependencies (e.g. unit tests that focus on other behavior).
func (r *Reconciler) backendExists(ctx context.Context, name, namespace string) bool {
	// Registry check first: the catalog is keyed by the namespace-qualified
	// "<ns>/<svc>" key (020 Part 1).
	regName := name
	if namespace != "" {
		regName = serviceref.New(namespace, name).Key()
	}
	if r.Sink != nil && r.Sink.HasRegistryService(regName) {
		return true
	}
	// Fall back to k8s Service existence check.
	if r.Client == nil {
		return true // no client → treat as admitted (safe degradation)
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return false // neither a registry service nor a k8s Service → BackendNotFound
		}
		// Transient error: admit the backend to avoid false-positive 500s.
	}
	return true
}

// buildHTTPRouteBackends converts the HTTPRoute rule's backendRefs into a list of
// RouteBackend values, applying Gateway API admission rules: non-core group/kind
// refs are skipped; ungranted cross-namespace refs are skipped (RefNotPermitted).
// Weight nil → default 1 per spec. Returns an empty slice when no backend is admissible.
//
// For each admitted backend it resolves the cleartext STRICT_DNS dial port via
// resolveDialPort: a HEADLESS Service's FQDN resolves to pod IPs (no kube-proxy in
// the path), so the edge must dial the Service's numeric targetPort rather than the
// service port. A mesh-registered or ClusterIP backend leaves DialPort 0 (the cache
// falls back to Port).
func (r *Reconciler) buildHTTPRouteBackends(ctx context.Context, refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) []cache.RouteBackend {
	out := make([]cache.RouteBackend, 0, len(refs))
	for _, b := range refs {
		if rb, ok := r.httpBackendRefToRouteBackend(ctx, b, routeNamespace, grants); ok {
			out = append(out, rb)
		}
	}
	return out
}

// httpBackendRefToRouteBackend converts a single HTTPBackendRef to a RouteBackend if admissible.
// Returns (backend, true) when admitted, (zero, false) when the ref should be skipped.
func (r *Reconciler) httpBackendRefToRouteBackend(ctx context.Context, b gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) (cache.RouteBackend, bool) {
	if b.Group != nil && string(*b.Group) != "" {
		return cache.RouteBackend{}, false
	}
	if b.Kind != nil && string(*b.Kind) != "Service" {
		return cache.RouteBackend{}, false
	}
	name := string(b.Name)
	if name == "" {
		return cache.RouteBackend{}, false
	}
	if !attachment.BackendPermitted(b.Namespace, routeNamespace, "HTTPRoute", name, grants) {
		return cache.RouteBackend{}, false
	}
	ns := routeNamespace
	if b.Namespace != nil && string(*b.Namespace) != "" {
		ns = string(*b.Namespace)
	}
	if !r.backendExists(ctx, name, ns) {
		return cache.RouteBackend{}, false
	}
	var port uint32
	if b.Port != nil {
		port = uint32(*b.Port)
	}
	weight := uint32(1)
	if b.Weight != nil {
		weight = uint32(*b.Weight)
	}
	return cache.RouteBackend{
		Service:          name,
		BackendNamespace: ns,
		Port:             port,
		Weight:           weight,
		DialPort:         r.resolveDialPort(ctx, ns, name, port),
	}, true
}

// resolveDialPort returns the TCP port the edge's cleartext STRICT_DNS cluster must
// connect to for a non-mesh backend Service. It only differs from servicePort for a
// HEADLESS Service (clusterIP: None): its FQDN resolves straight to pod IPs, so the
// edge must dial the numeric targetPort of the matching ServicePort instead of the
// service port (there is no kube-proxy VIP to remap port→targetPort). For a ClusterIP
// Service — or when the Service can't be read, the targetPort is named (we cannot
// resolve a named targetPort to a number without the pod spec), or there is no match —
// it returns 0, signalling the cache to fall back to servicePort (the existing,
// already-correct ClusterIP behavior). It never returns an error: a missing/unreadable
// Service degrades to the prior behavior rather than breaking the route.
func (r *Reconciler) resolveDialPort(ctx context.Context, namespace, service string, servicePort uint32) uint32 {
	if servicePort == 0 || r.Client == nil {
		return 0
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: service}, svc); err != nil {
		return 0 // unreadable/missing → fall back to servicePort (no change)
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		return 0 // ClusterIP Service: kube-proxy remaps port→targetPort; dial servicePort.
	}
	// Headless Service: find the ServicePort matching servicePort and dial its
	// numeric targetPort. A named (string) targetPort can't be resolved here without
	// the pod spec, so fall back to servicePort for that (uncommon) case.
	for _, sp := range svc.Spec.Ports {
		if uint32(sp.Port) != servicePort {
			continue
		}
		if tp := sp.TargetPort.IntValue(); tp > 0 {
			return uint32(tp)
		}
		// targetPort unset → defaults to the service port; named → unresolved here.
		return 0
	}
	return 0
}

// buildL4Backends converts a BackendRef slice into L4Backends with resolved TCP
// cluster names. Refs with a non-core group or non-Service kind are skipped, as are
// ungranted cross-namespace refs (RefNotPermitted: dropped from the data plane).
// routeKind is the referring route's kind (TCPRoute/TLSRoute).
//
// Edge L4 backends are mesh-only (the cache's edgeTCPClusters builds a registry
// TCP cluster per backend), so the backend's data-plane cluster and dependency key
// are the namespace-qualified "<ns>/<svc>" serviceref key (020 Part 1): the
// backendRef's own namespace when set, else the route's namespace.
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
		if !attachment.BackendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		ns := routeNamespace
		if bn := attachment.DerefBackendNamespace(b.Namespace); bn != "" {
			ns = bn
		}
		key := serviceref.New(ns, name).Key()
		backends = append(backends, proxy.L4Backend{
			Service: key,
			Cluster: proxy.TCPClusterName(key, r.MeshDomain),
			Weight:  weight,
		})
	}
	return backends
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

// buildRouteTimeout parses the HTTPRoute rule's timeouts.request field into a
// protobuf Duration for the edge route action. Returns nil when no timeout is set
// or the duration is zero/unparseable (GEP-2257: a zero-value timeout means "no
// timeout", and an invalid string is treated as unset rather than an error).
func buildRouteTimeout(timeouts *gatewayv1.HTTPRouteTimeouts) *durationpb.Duration {
	if timeouts == nil || timeouts.Request == nil {
		return nil
	}
	d, err := time.ParseDuration(string(*timeouts.Request))
	if err != nil || d <= 0 {
		return nil
	}
	return durationpb.New(d)
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
		applyRedirectPath(r, rd.Path)
		return r
	}
	return nil
}

// applyRedirectPath applies a path modifier to a GammaRedirect.
func applyRedirectPath(r *proxy.GammaRedirect, path *gatewayv1.HTTPPathModifier) {
	if path == nil {
		return
	}
	switch path.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		if path.ReplaceFullPath != nil {
			r.PathType = "ReplaceFullPath"
			r.PathValue = *path.ReplaceFullPath
		}
	case gatewayv1.PrefixMatchHTTPPathModifier:
		if path.ReplacePrefixMatch != nil {
			r.PathType = "ReplacePrefixMatch"
			r.PathValue = *path.ReplacePrefixMatch
		}
	}
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
		applyURLRewritePath(r, rw.Path)
		return r
	}
	return nil
}

// applyURLRewritePath applies a path modifier to a GammaURLRewrite.
func applyURLRewritePath(r *proxy.GammaURLRewrite, path *gatewayv1.HTTPPathModifier) {
	if path == nil {
		return
	}
	switch path.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		if path.ReplaceFullPath != nil {
			r.PathType = "ReplaceFullPath"
			r.PathValue = *path.ReplaceFullPath
		}
	case gatewayv1.PrefixMatchHTTPPathModifier:
		if path.ReplacePrefixMatch != nil {
			r.PathType = "ReplacePrefixMatch"
			r.PathValue = *path.ReplacePrefixMatch
		}
	}
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
			applyRequestHeaderModifier(m, f.RequestHeaderModifier)
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier == nil {
				continue
			}
			ensure()
			applyResponseHeaderModifier(m, f.ResponseHeaderModifier)
			// Redirect, rewrite, mirror: handled by dedicated builders above; skip here.
		}
	}
	return m
}

func applyRequestHeaderModifier(m *proxy.GammaHeaderMutation, mod *gatewayv1.HTTPHeaderFilter) {
	for _, h := range mod.Set {
		m.SetRequest = append(m.SetRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
	}
	for _, h := range mod.Add {
		m.AddRequest = append(m.AddRequest, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
	}
	m.RemoveRequest = append(m.RemoveRequest, mod.Remove...)
}

func applyResponseHeaderModifier(m *proxy.GammaHeaderMutation, mod *gatewayv1.HTTPHeaderFilter) {
	for _, h := range mod.Set {
		m.SetResponse = append(m.SetResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
	}
	for _, h := range mod.Add {
		m.AddResponse = append(m.AddResponse, proxy.GammaHeaderKV{Name: string(h.Name), Value: h.Value})
	}
	m.RemoveResponse = append(m.RemoveResponse, mod.Remove...)
}

// boolPtr returns a pointer to the given bool value. Used to set
// controller.Options.NeedLeaderElection without importing k8s.io/utils/ptr.
func boolPtr(b bool) *bool { return &b }
