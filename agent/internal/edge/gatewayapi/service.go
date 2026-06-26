package gatewayapi

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/bpalermo/aether/agent/internal/edge/portalloc"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// LabelEdgeGateway is the label applied to per-Gateway LoadBalancer Services for
	// GC (value = "<namespace>.<name>"). The edge reconciler lists Services with this
	// label to find stale per-Gateway Services and delete them when their Gateway is
	// gone.
	LabelEdgeGateway = "aether.io/edge-gateway"

	// AnnotationMetalLBLoadBalancerIPs is the MetalLB annotation that pins a
	// LoadBalancer Service to a specific IP from the MetalLB pool.
	AnnotationMetalLBLoadBalancerIPs = "metallb.universe.tf/loadBalancerIPs"
)

// edgeSelectorLabels are the pod-selector labels for edge pods — the same set the
// edge Deployment's pod template carries. Per-Gateway LoadBalancer Services select
// these pods so traffic reaches any edge replica.
var edgeSelectorLabels = map[string]string{
	"app.kubernetes.io/name":      "aether-edge",
	"app.kubernetes.io/component": "edge",
}

// gatewayServiceName returns the name of the per-Gateway LoadBalancer Service.
// Scheme: <edgeFullName>-gw-<ns>-<gwname>, truncated to 63 chars (DNS label).
func gatewayServiceName(edgeServiceName, namespace, gatewayName string) string {
	s := fmt.Sprintf("%s-gw-%s-%s", edgeServiceName, namespace, gatewayName)
	if len(s) > 63 {
		// Truncate to 63 chars; unlikely in practice for normal names.
		s = s[:63]
	}
	return s
}

// gatewayLabelValue returns the label value for a per-Gateway Service.
// Format: "<namespace>.<name>" — uniquely identifies a Gateway.
func gatewayLabelValue(namespace, name string) string {
	return namespace + "." + name
}

// gatewayListenerAllocation holds the allocation for ONE external port of a Gateway.
// Listeners that share an external port share one internal port + one edge listener
// (which demuxes by hostname/SNI), so this is keyed per external port, not per
// listener — a Gateway with several listeners on :80 yields ONE allocation (and one
// Service port). Per-listener allocation emitted duplicate Service ports, which k8s
// rejects ("Duplicate value: port-80"), leaving the Gateway un-projected (empty
// route table → 404).
type gatewayListenerAllocation struct {
	externalPort   uint32
	internalPort   uint32
	tlsSecretNames []string // merged TLS cert SDS names for this port (empty = plain HTTP)
}

// allocateGatewayListenerPorts runs the port allocator once per (Gateway, external
// port) across all Gateways and returns, per Gateway, ONE allocation per external
// port with the listeners' TLS certs merged. Listeners sharing an external port
// share one internal port + one edge listener; allocating per listener would emit
// duplicate Service ports (rejected by k8s) and drop the Gateway.
func allocateGatewayListenerPorts(gws []gatewayv1.Gateway, hostCerts map[string]string) (map[gatewayKey][]gatewayListenerAllocation, error) {
	portKey := func(ns, name string, port uint32) portalloc.Key {
		return portalloc.Key{Namespace: ns, GatewayName: name, ListenerName: fmt.Sprintf("port-%d", port)}
	}

	// One allocator key per (Gateway, external port).
	var keys []portalloc.Key
	seen := map[portalloc.Key]struct{}{}
	for _, gw := range gws {
		for _, ln := range gw.Spec.Listeners {
			k := portKey(gw.Namespace, gw.Name, uint32(ln.Port))
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	ports, err := portalloc.AssignAll(keys)
	if err != nil {
		return nil, fmt.Errorf("per-Gateway port allocation: %w", err)
	}

	result := make(map[gatewayKey][]gatewayListenerAllocation, len(gws))
	for _, gw := range gws {
		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}
		// Group listeners by external port, unioning their certs (an HTTPS port may
		// host several listeners with different hostnames/certs — SNI selects).
		certsByPort := map[uint32]map[string]struct{}{}
		var portOrder []uint32
		for _, ln := range gw.Spec.Listeners {
			ep := uint32(ln.Port)
			if _, ok := certsByPort[ep]; !ok {
				certsByPort[ep] = map[string]struct{}{}
				portOrder = append(portOrder, ep)
			}
			if c := listenerCert(ln, hostCerts); c != "" {
				certsByPort[ep][c] = struct{}{}
			}
		}
		allocs := make([]gatewayListenerAllocation, 0, len(portOrder))
		for _, ep := range portOrder {
			certSet := certsByPort[ep]
			certs := make([]string, 0, len(certSet))
			for c := range certSet {
				certs = append(certs, c)
			}
			sort.Strings(certs)
			allocs = append(allocs, gatewayListenerAllocation{
				externalPort:   ep,
				internalPort:   ports[portKey(gw.Namespace, gw.Name, ep)],
				tlsSecretNames: certs,
			})
		}
		result[gk] = allocs
	}
	return result, nil
}

// listenerCert resolves the SDS cert name for one listener from the already-resolved
// hostCerts: by the listener's hostname, falling back to the catch-all ("") cert.
// Plain-HTTP listeners (no TLS) return "".
func listenerCert(ln gatewayv1.Listener, hostCerts map[string]string) string {
	if ln.TLS == nil {
		return ""
	}
	if ln.Hostname != nil {
		if c, ok := hostCerts[string(*ln.Hostname)]; ok {
			return c
		}
	}
	return hostCerts[""]
}

// reconcileGatewayServices creates/updates the per-Gateway LoadBalancer Service
// for each class-aether Gateway, and garbage-collects stale per-Gateway Services
// whose Gateway no longer exists. Returns a map from gatewayKey → the Service's
// assigned IP (from status.loadBalancer.ingress), used for status.addresses.
//
// The production Gateway (whichever has spec.addresses set, or the one matching
// the existing shared edge Service) pins its IP via the MetalLB annotation. All
// other Gateways get auto-assigned IPs from the MetalLB pool.
func (r *Reconciler) reconcileGatewayServices(
	ctx context.Context,
	ourGateways []gatewayv1.Gateway,
	allocations map[gatewayKey][]gatewayListenerAllocation,
) (map[gatewayKey]string, error) {
	assignedIPs := make(map[gatewayKey]string, len(ourGateways))
	currentGWKeys := make(map[string]struct{}, len(ourGateways))

	for _, gw := range ourGateways {
		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}
		allocs := allocations[gk]
		svcName := gatewayServiceName(r.EdgeServiceName, gw.Namespace, gw.Name)
		currentGWKeys[svcName] = struct{}{}

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: r.Namespace,
			},
		}
		// Determine the pinned IP (MetalLB annotation) if spec.addresses is set.
		pinnedIP := ""
		for _, addr := range gw.Spec.Addresses {
			if addr.Type != nil && *addr.Type == gatewayv1.IPAddressType && addr.Value != "" {
				pinnedIP = addr.Value
				break
			}
		}

		// Build the desired port list from the allocation. Dedup by external port:
		// a Service may have at most ONE ServicePort per (name, port) tuple — k8s
		// rejects duplicates ("Duplicate value: port-80"), which drops the whole
		// Service and leaves the Gateway without a status address. allocs is already
		// one-per-external-port (allocateGatewayListenerPorts groups by external port),
		// but we dedup here as a defensive backstop so any caller — present or future —
		// that passes multiple allocations for the same external port still yields a
		// single ServicePort rather than a Service the API server refuses.
		ports := make([]corev1.ServicePort, 0, len(allocs))
		seenPort := make(map[uint32]struct{}, len(allocs))
		for _, a := range allocs {
			if _, dup := seenPort[a.externalPort]; dup {
				continue
			}
			seenPort[a.externalPort] = struct{}{}
			portName := fmt.Sprintf("port-%d", a.externalPort)
			ports = append(ports, corev1.ServicePort{
				Name:       portName,
				Port:       int32(a.externalPort),
				TargetPort: intstr.FromInt32(int32(a.internalPort)),
				Protocol:   corev1.ProtocolTCP,
			})
		}
		if len(ports) == 0 {
			// No listeners — skip creating a Service for this Gateway.
			continue
		}

		// CreateOrUpdate the per-Gateway Service.
		err := r.createOrUpdateGatewayService(ctx, svc, gw.Namespace, gw.Name, ports, pinnedIP)
		if err != nil {
			r.Log.WarnContext(ctx, "failed to reconcile per-Gateway Service",
				"gateway", gw.Namespace+"/"+gw.Name, "service", svcName, "error", err.Error())
			continue
		}

		// Read the (possibly just-created) Service's assigned LB IP.
		ip := r.readServiceLBIP(ctx, svcName)
		if ip != "" {
			assignedIPs[gk] = ip
		}
	}

	// GC: delete per-Gateway Services whose Gateway no longer exists. List all
	// Services in the edge namespace with the LabelEdgeGateway label.
	if err := r.gcStaleGatewayServices(ctx, currentGWKeys); err != nil {
		r.Log.WarnContext(ctx, "per-Gateway Service GC error", "error", err.Error())
	}

	return assignedIPs, nil
}

// createOrUpdateGatewayService creates or updates one per-Gateway LoadBalancer
// Service. It applies the gateway selector labels, the per-Gateway label for GC,
// the MetalLB IP annotation (if pinnedIP is set), and the port mapping.
func (r *Reconciler) createOrUpdateGatewayService(
	ctx context.Context,
	svc *corev1.Service,
	gwNamespace, gwName string,
	ports []corev1.ServicePort,
	pinnedIP string,
) error {
	// apply stamps the desired labels/annotations/spec onto a Service object.
	apply := func(s *corev1.Service) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[LabelEdgeGateway] = gatewayLabelValue(gwNamespace, gwName)
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		if pinnedIP != "" {
			s.Annotations[AnnotationMetalLBLoadBalancerIPs] = pinnedIP
		} else {
			delete(s.Annotations, AnnotationMetalLBLoadBalancerIPs)
		}
		s.Spec.Type = corev1.ServiceTypeLoadBalancer
		s.Spec.Selector = edgeSelectorLabels
		s.Spec.Ports = ports
	}

	key := types.NamespacedName{Namespace: r.Namespace, Name: svc.Name}
	existing := &corev1.Service{}
	err := r.Get(ctx, key, existing)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("get per-Gateway Service %s: %w", svc.Name, err)
	}

	if errors.IsNotFound(err) {
		apply(svc)
		if cerr := r.Create(ctx, svc); cerr != nil {
			// Idempotency: a concurrent reconcile (stale cache) may have created it
			// between our Get and Create — re-fetch and fall through to the update
			// path instead of surfacing an AlreadyExists error (reconcile churn).
			if !errors.IsAlreadyExists(cerr) {
				return fmt.Errorf("create per-Gateway Service %s: %w", svc.Name, cerr)
			}
			if gerr := r.Get(ctx, key, existing); gerr != nil {
				return fmt.Errorf("get per-Gateway Service %s after create race: %w", svc.Name, gerr)
			}
		} else {
			r.Log.InfoContext(ctx, "created per-Gateway LoadBalancer Service",
				"gateway", gwNamespace+"/"+gwName, "service", svc.Name, "pinnedIP", pinnedIP)
			return nil
		}
	}

	updated := existing.DeepCopy()
	apply(updated)
	if uerr := r.Update(ctx, updated); uerr != nil {
		return fmt.Errorf("update per-Gateway Service %s: %w", svc.Name, uerr)
	}
	return nil
}

// readServiceLBIP returns the first LoadBalancer ingress IP (or hostname) of the
// named Service in r.Namespace. Returns "" when the Service hasn't been assigned
// an address yet (MetalLB is still allocating).
func (r *Reconciler) readServiceLBIP(ctx context.Context, svcName string) string {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: svcName}, svc); err != nil {
		return ""
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}

// gcStaleGatewayServices deletes per-Gateway LoadBalancer Services in r.Namespace
// that have the LabelEdgeGateway label but are NOT in currentSvcNames (i.e. their
// Gateway has been deleted or is no longer of our class).
func (r *Reconciler) gcStaleGatewayServices(ctx context.Context, currentSvcNames map[string]struct{}) error {
	svcList := &corev1.ServiceList{}
	// client.HasLabels matches any Service that has the label key, regardless of value.
	if err := r.List(ctx, svcList,
		client.InNamespace(r.Namespace),
		client.HasLabels{LabelEdgeGateway},
	); err != nil {
		return fmt.Errorf("list per-Gateway Services for GC: %w", err)
	}
	for i := range svcList.Items {
		s := &svcList.Items[i]
		if _, current := currentSvcNames[s.Name]; current {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !errors.IsNotFound(err) {
			r.Log.WarnContext(ctx, "failed to delete stale per-Gateway Service",
				"service", s.Name, "error", err.Error())
		} else {
			r.Log.InfoContext(ctx, "deleted stale per-Gateway Service", "service", s.Name)
		}
	}
	return nil
}

// buildEdgeGatewayEntries constructs the []cache.EdgeGatewayEntry that the
// snapshot cache needs to emit per-Gateway listeners and route configs.
// It takes the already-projected virtualHosts (the result of the existing
// HTTP-route projection loop), filters them per-Gateway by hostname intersection,
// and pairs them with listener port allocations.
//
// In Phase 2 each Gateway gets the virtual hosts whose TLSSecret matches the
// Gateway's listener cert (or all HTTP vhosts for an HTTP Gateway). This is a
// simplified assignment: each VirtualHost is placed on the Gateway whose
// listener covers its hosts (by the existing hostCerts resolution). VirtualHosts
// with no TLSSecret go to HTTP Gateways; those with a TLSSecret go to HTTPS Gateways.
func buildEdgeGatewayEntries(
	ourGateways []gatewayv1.Gateway,
	allVhosts []cache.VirtualHost,
	allocations map[gatewayKey][]gatewayListenerAllocation,
	gatewayHTTPRedirect map[gatewayKey]bool,
) []cache.EdgeGatewayEntry {
	entries := make([]cache.EdgeGatewayEntry, 0, len(ourGateways))

	for _, gw := range ourGateways {
		gk := gatewayKey{Namespace: gw.Namespace, Name: gw.Name}
		allocs := allocations[gk]
		if len(allocs) == 0 {
			continue
		}

		// Build the EdgeGatewayListenerEntry slice from allocations.
		redirect := gatewayHTTPRedirect[gk]
		lns := make([]cache.EdgeGatewayListenerEntry, 0, len(allocs))

		for _, a := range allocs {
			isHTTPRedirect := redirect && len(a.tlsSecretNames) == 0
			lns = append(lns, cache.EdgeGatewayListenerEntry{
				ExternalPort:   a.externalPort,
				InternalPort:   a.internalPort,
				TLSSecretNames: a.tlsSecretNames,
				HTTPRedirect:   isHTTPRedirect,
			})
		}

		// Assign vhosts to this Gateway by ATTACHMENT: a vhost goes to this Gateway's
		// route table iff the route attaches to it (vh.Gateways contains this Gateway).
		// A vhost with no recorded Gateways (legacy/Phase 1 fallback) attaches to all.
		// Assigning by the vhost's cert tag was wrong: a plain-HTTP route could pick up
		// an unrelated Gateway's catch-all cert and then attach to ZERO Gateways
		// (present-but-empty route table → 404).
		gwKey := gw.Namespace + "/" + gw.Name
		var gwVhosts []cache.VirtualHost
		for _, vh := range allVhosts {
			if len(vh.Gateways) == 0 || slices.Contains(vh.Gateways, gwKey) {
				gwVhosts = append(gwVhosts, vh)
			}
		}

		entries = append(entries, cache.EdgeGatewayEntry{
			Namespace:    gw.Namespace,
			Name:         gw.Name,
			Listeners:    lns,
			VirtualHosts: gwVhosts,
		})
	}
	return entries
}
