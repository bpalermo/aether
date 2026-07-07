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

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	constants "github.com/bpalermo/aether/common/constants"
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
	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs and listener
	// certificateRefs (drop + RefNotPermitted). The edge cache is cluster-wide
	// (post-#323). Listed first so resolveGateways can enforce them on cert refs.
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := r.List(ctx, grantList); err != nil {
		return reconcile.Result{}, err
	}
	grants := grantList.Items

	gateways, ourGateways, gatewayListeners, perGWHostCerts, hostCerts, certs, tlsResults, err := r.resolveGateways(ctx, grants)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Build a map of Gateway key → listener hostnames so we can compute the
	// effective hostname intersection for each HTTPRoute (Gateway API §spec:
	// effective hostnames = route.Hostnames ∩ listener.Hostname; a route with no
	// hostnames inherits the listener's; a listener with no hostname admits all
	// route hostnames).
	gwListenerHostnames := buildGatewayListenerHostnames(ourGateways)

	// --- HTTPRoutes (cluster-wide) ---
	httpRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpRoutes); err != nil {
		return reconcile.Result{}, err
	}
	// Deterministic order so (a) same-hostname merge in the cache is stable and
	// (b) the Gateway API tie-break for routes of equal path specificity is correct:
	// oldest creationTimestamp first, then namespace, then name. The cache's stable
	// path-specificity sort preserves this order for equal-specificity routes.
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
		if !attachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, gateways) {
			continue
		}
		vh := r.buildVirtualHost(ctx, hr, hostCerts, grants)
		// A hostname-less route is NOT inert: per Gateway API it matches every host on
		// its listener, served via the cache's catch-all "*" vhost. Only an empty
		// route set (no backend/redirect produced a route) makes it skippable.
		if len(vh.Routes) == 0 {
			continue
		}
		// Scope the vhost to the Gateways the route actually attaches to (Phase 2
		// assigns by attachment, not by the vhost's cert tag).
		gwKeys := attachedGatewayKeys(hr.Spec.ParentRefs, hr.Namespace, gateways)
		vh.Gateways = gwKeys
		// Compute effective hostnames: route.Hostnames ∩ each attached listener's
		// hostnames. When a parentRef has a sectionName, intersect with that
		// listener only (not the whole Gateway). This makes hostname-less routes
		// inherit the specific listener's hostname and ensures routes with explicit
		// hostnames that don't match any listener are discarded rather than leaking
		// into the catch-all "*" vhost (where they would incorrectly match requests
		// to unrelated hosts — the no-intersecting-hosts 500 bug).
		hostnameLookupKeys := attachedHostnameLookupKeys(hr.Spec.ParentRefs, hr.Namespace, gateways)
		vh.Hosts = effectiveHostnames(vh.Hosts, hostnameLookupKeys, gwListenerHostnames)
		// If the route declared explicit hostnames but none intersect with any
		// attached listener, discard the vhost entirely. An empty effective-hostname
		// set means "no listener admits this route" — it must not produce routes at
		// all, not even in the catch-all "*" vhost.
		if len(hr.Spec.Hostnames) > 0 && len(vh.Hosts) == 0 {
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
		allocations, allocErr := allocateGatewayListenerPorts(ourGateways, perGWHostCerts)
		if allocErr != nil {
			r.Log.WarnContext(ctx, "per-Gateway port allocation failed, falling back to Phase 1",
				"error", allocErr.Error())
			// Fall through to Phase 1 path.
			r.Sink.SetEdgeGateways(nil)
		} else {
			edgeConfigs := make(map[gatewayKey]*configv1.EdgeConfigSpec, len(ourGateways))
			for i := range ourGateways {
				gw := &ourGateways[i]
				edgeConfigs[gatewayKey{Namespace: gw.Namespace, Name: gw.Name}] = r.resolveEdgeConfig(ctx, gw)
			}
			ips, svcErr := r.reconcileGatewayServices(ctx, ourGateways, allocations, edgeConfigs)
			if svcErr != nil {
				r.Log.WarnContext(ctx, "per-Gateway Service reconcile error", "error", svcErr.Error())
			}
			perGWAssignedIPs = ips
			entries := buildEdgeGatewayEntries(ourGateways, vhosts, allocations, gatewayHTTPRedirect, edgeConfigs)
			r.Sink.SetEdgeGateways(entries)
			r.Log.DebugContext(ctx, "projected per-Gateway entries",
				"gateways", len(entries), "ourGateways", len(ourGateways),
				"totalVhosts", len(vhosts))
			// In Phase 2 we still call SetVirtualHosts with the full vhosts so
			// the Phase 1 fallback route table is up-to-date (no-op at snapshot
			// time when per-Gateway mode is active, but keeps state consistent
			// for edge cases like a reconcile that loses the Phase 2 allocations).
		}
	} else {
		// Phase 1 / disabled: clear any stale Phase 2 state, and garbage-collect any
		// per-Gateway Services left over from a previous Phase 2 run. The Phase 2
		// reconcile path is skipped here, so without this the Services orphan and
		// keep holding their LoadBalancer IP (e.g. the pinned edge address), which
		// blocks the shared Service from reclaiming it on a downgrade.
		r.Sink.SetEdgeGateways(nil)
		if r.EdgeServiceName != "" {
			if err := r.gcStaleGatewayServices(ctx, map[string]struct{}{}); err != nil {
				r.Log.WarnContext(ctx, "GC of stale per-Gateway Services failed", "error", err.Error())
			}
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
		// Only core ("") group Secret kind is supported; any other group/kind is an
		// invalid certificateRef.
		if ref.Group != nil && string(*ref.Group) != "" {
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported group %q", ref.Name, *ref.Group)}
		}
		if ref.Kind != nil && string(*ref.Kind) != "Secret" {
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q has unsupported kind %q", ref.Name, *ref.Kind)}
		}
		name := string(ref.Name)
		// The Secret lives in certificateRef.namespace, defaulting to the GATEWAY's
		// own namespace — NOT the edge's namespace. A cross-namespace ref (namespace
		// set and != the Gateway's) is permitted only by a ReferenceGrant in the
		// Secret's namespace; otherwise it is RefNotPermitted (and not resolved).
		certNS := gw.Namespace
		if ref.Namespace != nil && string(*ref.Namespace) != "" {
			certNS = string(*ref.Namespace)
		}
		if referencegrant.CrossNamespace(certNS, gw.Namespace) &&
			!referencegrant.PermitsSecret(grants, gw.Namespace, certNS, name) {
			return listenerTLSResult{hasTLS: true, resolved: false, reason: string(gatewayv1.ListenerReasonRefNotPermitted), message: fmt.Sprintf("certificateRef %s/%s is not permitted by any ReferenceGrant", certNS, name)}
		}
		secretRef := certNS + "/" + name
		sdsName, sErr := r.Secrets.SDSName(configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, secretRef)
		if sErr != nil {
			r.Log.WarnContext(ctx, "gateway listener TLS provider unavailable", "gateway", gw.Name, "secret", secretRef, "error", sErr.Error())
			return listenerTLSResult{hasTLS: true, resolved: false, message: fmt.Sprintf("certificateRef %q provider unavailable", name)}
		}
		cert, cErr := r.Secrets.Resolve(ctx, configv1.SecretProvider_SECRET_PROVIDER_KUBERNETES, secretRef)
		if cErr != nil {
			r.Log.WarnContext(ctx, "gateway listener TLS cert unresolved", "gateway", gw.Name, "secret", secretRef, "error", cErr.Error())
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
		redirect := buildHTTPRedirect(rule.Filters)
		urlRewrite := buildHTTPURLRewrite(rule.Filters)
		timeout := buildRouteTimeout(rule.Timeouts)

		// Collect ALL admissible backends in this rule with their weights.
		// backendRef.weight nil → default 1 (per Gateway API spec).
		backends := r.buildHTTPRouteBackends(ctx, rule.BackendRefs, hr.Namespace, grants)

		// A rule must have either at least one backend or a redirect to produce a route.
		// Exception: a rule where all backends are UNRESOLVABLE (InvalidKind /
		// BackendNotFound / RefNotPermitted) must still produce a route that returns
		// HTTP 500 — the Gateway API data-plane contract for invalid backends.
		// Detect this: no admissible backends (buildHTTPRouteBackends filtered them
		// all out) AND no redirect AND the rule actually has declared backendRefs
		// (so it is "invalid" rather than "empty").
		if redirect == nil {
			if len(backends) == 0 && len(rule.BackendRefs) > 0 {
				// All backendRefs are unresolvable (invalid kind, missing Service, or
				// ungranted cross-namespace ref). Check via backendsResolveL4 to confirm
				// (it reuses the same admission logic used by the status writer), then
				// emit a fixed 500 route with the path/header match still applied.
				resolved, _, _ := r.backendsResolveHTTP(ctx, hr.Namespace, []gatewayv1.HTTPRouteRule{rule}, grants)
				if !resolved {
					mutation := buildEdgeHTTPHeaderMutation(rule.Filters)
					if len(rule.Matches) == 0 {
						vh.Routes = append(vh.Routes, cache.Route{
							Prefix:               "/",
							HeaderMutation:       mutation,
							DirectResponseStatus: 500,
						})
					} else {
						for _, m := range rule.Matches {
							prefix, exact := "/", ""
							if m.Path != nil && m.Path.Value != nil {
								switch ptrType(m.Path.Type, gatewayv1.PathMatchPathPrefix) {
								case gatewayv1.PathMatchExact:
									exact = *m.Path.Value
									prefix = ""
								default:
									prefix = *m.Path.Value
								}
							}
							var hdrs []proxy.RouteHeaderMatch
							for _, h := range m.Headers {
								hm := proxy.RouteHeaderMatch{Name: string(h.Name), Value: h.Value}
								if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
									hm.Regex = true
								}
								hdrs = append(hdrs, hm)
							}
							method := ""
							if m.Method != nil {
								method = string(*m.Method)
							}
							var qps []proxy.RouteQueryParamMatch
							for _, q := range m.QueryParams {
								qm := proxy.RouteQueryParamMatch{Name: string(q.Name), Value: q.Value}
								if q.Type != nil && *q.Type == gatewayv1.QueryParamMatchRegularExpression {
									qm.Regex = true
								}
								qps = append(qps, qm)
							}
							vh.Routes = append(vh.Routes, cache.Route{
								Prefix:               prefix,
								Exact:                exact,
								Headers:              hdrs,
								Method:               method,
								QueryParams:          qps,
								HeaderMutation:       mutation,
								DirectResponseStatus: 500,
							})
						}
					}
					continue
				}
			} else if len(backends) == 0 {
				// No backends, no redirect, no declared backendRefs → skip (inert rule).
				continue
			}
		}

		mutation := buildEdgeHTTPHeaderMutation(rule.Filters)

		// For backward-compat we also populate the legacy Service/Port/BackendNamespace
		// from the first backend so that code paths that read those fields still work.
		var legacySvc, legacyNS string
		var legacyPort, legacyDialPort uint32
		if len(backends) > 0 {
			legacySvc = backends[0].Service
			legacyPort = backends[0].Port
			legacyNS = backends[0].BackendNamespace
			legacyDialPort = backends[0].DialPort
		}

		if len(rule.Matches) == 0 {
			vh.Routes = append(vh.Routes, cache.Route{
				Prefix:           "/",
				Exact:            "",
				Service:          legacySvc,
				Port:             legacyPort,
				BackendNamespace: legacyNS,
				DialPort:         legacyDialPort,
				Backends:         backends,
				HeaderMutation:   mutation,
				Redirect:         redirect,
				URLRewrite:       urlRewrite,
				Timeout:          timeout,
			})
			continue
		}
		for _, m := range rule.Matches {
			prefix, exact := "/", ""
			if m.Path != nil && m.Path.Value != nil {
				switch ptrType(m.Path.Type, gatewayv1.PathMatchPathPrefix) {
				case gatewayv1.PathMatchExact:
					exact = *m.Path.Value
					prefix = ""
				default: // PathPrefix (and unimplemented types) → prefix
					prefix = *m.Path.Value
				}
			}

			// Build per-match header predicates.
			var hdrs []proxy.RouteHeaderMatch
			for _, h := range m.Headers {
				hm := proxy.RouteHeaderMatch{Name: string(h.Name), Value: h.Value}
				if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
					hm.Regex = true
				}
				hdrs = append(hdrs, hm)
			}

			// Method match: convert to uppercase string (Gateway API enum).
			method := ""
			if m.Method != nil {
				method = string(*m.Method)
			}

			// Build per-match query-parameter predicates.
			var qps []proxy.RouteQueryParamMatch
			for _, q := range m.QueryParams {
				qm := proxy.RouteQueryParamMatch{Name: string(q.Name), Value: q.Value}
				if q.Type != nil && *q.Type == gatewayv1.QueryParamMatchRegularExpression {
					qm.Regex = true
				}
				qps = append(qps, qm)
			}

			vh.Routes = append(vh.Routes, cache.Route{
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
				Headers:          hdrs,
				Method:           method,
				QueryParams:      qps,
				Timeout:          timeout,
			})
		}
	}
	if cert := certForHosts(vh.Hosts, hostCerts); cert != "" {
		vh.TLSSecret = cert
	}
	return vh
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
		if b.Group != nil && string(*b.Group) != "" {
			continue // only core Services
		}
		if b.Kind != nil && string(*b.Kind) != "Service" {
			continue
		}
		name := string(b.Name)
		if name == "" {
			continue
		}
		if !backendPermitted(b.Namespace, routeNamespace, "HTTPRoute", name, grants) {
			continue
		}
		ns := routeNamespace
		if b.Namespace != nil && string(*b.Namespace) != "" {
			ns = string(*b.Namespace)
		}
		// Registry-aware existence check: a backend is admissible if it is a
		// mesh/registry service OR a real k8s Service. Only drop on confirmed
		// non-existence in both (BackendNotFound). Transient API errors leave the
		// backend admitted to avoid false-positive 500s on API server hiccups.
		if !r.backendExists(ctx, name, ns) {
			continue // BackendNotFound: drop from admitted set
		}
		var port uint32
		if b.Port != nil {
			port = uint32(*b.Port)
		}
		weight := uint32(1) // default per Gateway API spec
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		out = append(out, cache.RouteBackend{
			Service:          name,
			BackendNamespace: ns,
			Port:             port,
			Weight:           weight,
			DialPort:         r.resolveDialPort(ctx, ns, name, port),
		})
	}
	return out
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

// attachedGatewayKeys returns the "<ns>/<name>" keys of OUR Gateways a route's
// parentRefs attach to. Used to scope a vhost to its actual Gateways' route tables
// (Phase 2 assignment by attachment, not by cert).
func attachedGatewayKeys(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[gatewayKey]struct{}) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		key := gatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[key]; !ok {
			continue
		}
		s := key.Namespace + "/" + key.Name
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		keys = append(keys, s)
	}
	return keys
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
		if !backendPermitted(b.Namespace, routeNamespace, routeKind, name, grants) {
			continue
		}
		weight := uint32(1)
		if b.Weight != nil {
			weight = uint32(*b.Weight)
		}
		ns := routeNamespace
		if bn := derefBackendNamespace(b.Namespace); bn != "" {
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

// firstBackendNamespace returns the resolved namespace of the first admissible
// (same-namespace or granted cross-namespace) backendRef. Same-namespace refs
// default to the route's own namespace when backendRef.Namespace is unset.
// Matches the selection logic of firstBackendService so they return data for
// the same ref.
func firstBackendNamespace(refs []gatewayv1.HTTPBackendRef, routeNamespace string, grants []gatewayv1beta1.ReferenceGrant) string {
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
		if b.Namespace != nil && string(*b.Namespace) != "" {
			return string(*b.Namespace)
		}
		return routeNamespace
	}
	return routeNamespace
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

// buildGatewayListenerHostnames extracts the listener hostnames for each Gateway
// into a map keyed by "<namespace>/<name>". A listener with no hostname is
// represented by an empty string "". The returned value is used to compute
// effective route hostnames via effectiveHostnames.
//
// Keys: "ns/name" for the per-Gateway union (used by routes without a
// sectionName), and "ns/name/sectionName" for per-section lookup (used when
// a parentRef specifies a sectionName). The per-section value contains only
// that listener's hostname so effectiveHostnames scopes the route to exactly
// the hostnames of the listener it attached to.
func buildGatewayListenerHostnames(gws []gatewayv1.Gateway) map[string][]string {
	out := make(map[string][]string, len(gws))
	for _, gw := range gws {
		gwKey := gw.Namespace + "/" + gw.Name
		seen := make(map[string]struct{}, len(gw.Spec.Listeners))
		var hostnames []string
		for _, ln := range gw.Spec.Listeners {
			h := ""
			if ln.Hostname != nil {
				h = string(*ln.Hostname)
			}
			// Per-section key: "ns/name/sectionName" → exactly this listener's hostname.
			sectionKey := gwKey + "/" + string(ln.Name)
			out[sectionKey] = []string{h}
			// Per-gateway key: union of all listener hostnames (deduplicated).
			if _, ok := seen[h]; !ok {
				seen[h] = struct{}{}
				hostnames = append(hostnames, h)
			}
		}
		out[gwKey] = hostnames
	}
	return out
}

// attachedHostnameLookupKeys returns the hostname-lookup keys for
// effectiveHostnames. When a parentRef specifies a sectionName the key is
// "ns/name/sectionName" so effectiveHostnames uses only that listener's
// hostname; otherwise it uses the gateway-level "ns/name" key (union of all
// listener hostnames). This implements Gateway API §hostname-intersection:
// "If the listener section name and/or port is specified, Hostnames must match
// only that listener."
func attachedHostnameLookupKeys(parentRefs []gatewayv1.ParentReference, routeNamespace string, gateways map[gatewayKey]struct{}) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, p := range parentRefs {
		if !parentRefIsGateway(p) {
			continue
		}
		gk := gatewayKey{Namespace: parentRefNamespace(p.Namespace, routeNamespace), Name: string(p.Name)}
		if _, ok := gateways[gk]; !ok {
			continue
		}
		gwKeyStr := gk.Namespace + "/" + gk.Name
		var lookupKey string
		if p.SectionName != nil && *p.SectionName != "" {
			lookupKey = gwKeyStr + "/" + string(*p.SectionName)
		} else {
			lookupKey = gwKeyStr
		}
		if _, dup := seen[lookupKey]; !dup {
			seen[lookupKey] = struct{}{}
			keys = append(keys, lookupKey)
		}
	}
	return keys
}

// effectiveHostnames computes the Gateway API effective hostname set for a route:
//
//	effective = ∪(over attached Gateways) of { route.Hostnames ∩ listenerHostnames(gw) }
//
// Gateway API intersection rules (per spec):
//   - A listener hostname "" (no hostname) admits ALL route hostnames unchanged.
//   - A route hostname "" (no hostnames on the route) inherits the listener's
//     hostname(s); a listener "" × route "" = "*" (catch-all, no constraint).
//   - exact == exact: they match; result is that exact hostname.
//   - listener "*.example.com" ∩ route "a.example.com": route is more specific → result "a.example.com".
//   - route "*.example.com" ∩ listener "a.example.com": listener is more specific → result "a.example.com".
//
// The return value replaces vh.Hosts in the reconciler so downstream code (the
// cache's buildEdgeVhostsLocked) sees only the hosts the route ACTUALLY matches.
// A nil/empty return means the route has no valid attachment (no listener admits
// any of its declared hosts) and must be discarded by the caller.
//
// Multi-Gateway: takes the union of intersections across all attached Gateways,
// deduplicating. This is a correct first cut (see implementation note).
func effectiveHostnames(routeHosts []string, gwKeys []string, gwListenerHostnames map[string][]string) []string {
	seen := make(map[string]struct{})
	var result []string
	add := func(h string) {
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			result = append(result, h)
		}
	}

	for _, gwKey := range gwKeys {
		lnHostnames, ok := gwListenerHostnames[gwKey]
		if !ok {
			// Unknown Gateway — should not happen (key was from attachedGatewayKeys).
			continue
		}
		for _, lh := range lnHostnames {
			if lh == "" {
				// Listener has no hostname restriction: admit all route hostnames.
				if len(routeHosts) == 0 {
					// route "" + listener "" = "*" (true catch-all). A no-hostname
					// listener with a no-hostname route admits ALL hosts. Return nil
					// immediately — no specific hostname from any other listener in
					// this key's union can narrow a catch-all back down. The caller
					// treats nil/empty Hosts as the "*" catch-all vhost.
					//
					// Previously this was `continue` (add nothing and keep processing),
					// which caused a multi-listener Gateway (e.g. one listener with no
					// hostname + two listeners with specific hostnames) to produce a
					// Hosts set of only the specific listener hostnames — silently
					// dropping the catch-all listener's "admit all" contribution. A
					// request like Host:example.org matched neither specific hostname,
					// fell through to the 404 catch-all vhost, and returned 404 instead
					// of the expected redirect. (HTTPRouteRedirectPortAndScheme, 443 subtests)
					return nil
				}
				for _, rh := range routeHosts {
					add(rh)
				}
				continue
			}
			// Listener has a hostname (exact or wildcard).
			if len(routeHosts) == 0 {
				// Route has no hostname constraint: inherits the listener's hostname.
				add(lh)
				continue
			}
			// Intersect each route hostname against this listener hostname.
			for _, rh := range routeHosts {
				if h, ok := hostnameIntersect(lh, rh); ok {
					add(h)
				}
			}
		}
	}
	return result
}

// boolPtr returns a pointer to the given bool value. Used to set
// controller.Options.NeedLeaderElection without importing k8s.io/utils/ptr.
func boolPtr(b bool) *bool { return &b }

// hostnameIntersect returns the more-specific of two hostnames if they match,
// and reports whether they intersect at all. The Gateway API rules are:
//   - exact == exact → match, result = that hostname.
//   - "*.example.com" vs "a.example.com" → match (specific wins), result = "a.example.com".
//   - "*.example.com" vs "*.other.com" → no match.
//   - "*.example.com" vs "*.example.com" → match, result = "*.example.com".
//   - "" (no hostname = catch-all) matches everything — callers must handle the
//     empty-string case before calling this function.
func hostnameIntersect(a, b string) (string, bool) {
	if a == b {
		return a, true
	}
	// a is a wildcard, b is specific: a matches b if b ends with a[1:].
	if strings.HasPrefix(a, "*.") && !strings.HasPrefix(b, "*.") {
		if strings.HasSuffix(b, a[1:]) {
			return b, true // b is more specific
		}
		return "", false
	}
	// b is a wildcard, a is specific.
	if strings.HasPrefix(b, "*.") && !strings.HasPrefix(a, "*.") {
		if strings.HasSuffix(a, b[1:]) {
			return a, true // a is more specific
		}
		return "", false
	}
	// Both are exact but different, or both wildcards but different — no match.
	return "", false
}
