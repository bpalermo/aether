// Package capture contains the node agent's transparent-capture controller: it
// watches the generated selectorless mesh Services (proposal 018, Phase 3a) and
// projects their cluster.local authorities into the snapshot cache, which builds the
// cap_http route table the per-pod capture listeners serve. Endpoints stay in the
// registry; this only maps a captured authority to its existing service cluster.
package capture

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"aethermesh.dev/common/serviceref"

	aetherannotations "aethermesh.dev/common/constants/annotations"
	aetherlabels "aethermesh.dev/common/constants/labels"
	commonlog "aethermesh.dev/common/log"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CaptureTCPService describes a non-HTTP mesh Service that needs a per-ClusterIP
// TCP-proxy floor chain on the capture listener (proposal 018, Phase 3a TCP floor).
// The ClusterIP drives the filter-chain prefix_ranges match; the cache derives the
// cluster name from ServiceName and its configured mesh domain.
type CaptureTCPService struct {
	// ServiceName is the bare mesh service name (registry key). The cache uses this
	// to derive the TCP cluster name (TCPClusterName) and to look up SAN namespaces.
	ServiceName string
	// ClusterIP is the k8s Service ClusterIP the capture listener's filter chain
	// matches on (original-dst recovered via SO_ORIGINAL_DST). Must be a valid
	// non-None IP address (headless/unallocated Services are skipped).
	ClusterIP string
	// PrimaryIsTCP reports whether the service's PRIMARY port is raw TCP, from
	// the registrar-stamped aether.io/app-protocol annotation.
	//
	// It gates the PORTLESS /32 floor chain and nothing else (proposal 037
	// design (d)). An HTTP-primary service must not have one — a per-IP chain
	// outranks the HCM's application-protocol match and would swallow the VIP's
	// HTTP traffic — but it may still serve raw-TCP ports, which get
	// destination_port-qualified chains that do not.
	PrimaryIsTCP bool
}

// AuthoritySink receives the projections from the generated mesh Services:
//   - SetCaptureAuthorities: service -> cluster.local FQDN (cap_http, transparent capture).
//   - SetMeshDNSRecords:     service -> mesh-Service ClusterIP (the per-pod dns_filter's
//     A record for <svc>.<meshDomain>, the mesh-global FQDN).
//   - SetCaptureTCPServices: the non-HTTP services that need per-ClusterIP TCP floor chains.
//
// All are emitted every reconcile; the cache uses whichever feature is enabled.
type AuthoritySink interface {
	SetCaptureAuthorities(authorities map[string]string)
	SetMeshDNSRecords(records map[string]string)
	SetCaptureTCPServices(services []CaptureTCPService)
}

// Reconciler watches the generated mesh Services (labeled aether.io/mesh-service) and
// replaces, on any change, the cache's service -> cluster.local authority map.
// Level-based: each reconcile re-lists, so adds/updates/deletes converge.
type Reconciler struct {
	client.Client

	Sink AuthoritySink
	Log  *slog.Logger
}

// SetupWithManager registers the reconciler to watch mesh Services only.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "capture")
	meshService := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[aetherlabels.LabelMeshService] == "true"
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(meshService)).
		Named("capture").
		Complete(r)
}

// isHTTPAppProtocol reports whether the annotation value represents an HTTP-family
// protocol (http, h2, grpc) that the HCM filter chain handles. Non-HTTP protocols
// (tcp, or any unrecognised value) get a per-ClusterIP TCP-proxy floor chain.
func isHTTPAppProtocol(proto string) bool {
	switch strings.ToLower(proto) {
	case "http", "h2", "grpc", "http2", "":
		return true
	default:
		return false
	}
}

// isUDPAppProtocol reports whether the service's primary port speaks datagrams.
//
// This is NOT the complement of isHTTPAppProtocol, and the difference matters: a
// UDP service must get neither an HCM chain nor a portless /32 TCP floor chain.
// Its data path is the udp_proxy capture listener, which is a separate,
// connection-less listener on the same port and shares no filter chain with the
// TCP side.
//
// Without this carve-out "udp" fell to isHTTPAppProtocol's default branch and
// was classified TCP-primary, so a UDP service's VIP acquired a portless TCP
// floor chain naming a tcp: cluster that may not exist. An unrecognised value
// still classifies as TCP -- that conservative default is deliberate and
// unchanged; only a protocol the mesh actually knows is datagram-only is carved
// out of it.
func isUDPAppProtocol(proto string) bool {
	return strings.EqualFold(proto, aetherannotations.ProtocolUDP)
}

// Reconcile re-lists the mesh Services and projects their cluster.local authorities,
// DNS records, and TCP-floor service set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, client.MatchingLabels{aetherlabels.LabelMeshService: "true"}); err != nil {
		return reconcile.Result{}, err
	}

	authorities := make(map[string]string, len(list.Items))
	records := make(map[string]string, len(list.Items))
	var tcpServices []CaptureTCPService
	for i := range list.Items {
		s := &list.Items[i]
		name := s.Annotations[aetherlabels.AnnotationMeshService]
		if name == "" {
			name = s.Name
		}
		// 020 Part 1: the service key is namespace-qualified "<ns>/<svc>"; the mesh
		// Service lives in <ns> with the bare ServiceAccount name.
		svc := serviceref.New(s.Namespace, name).Key()
		authorities[svc] = fmt.Sprintf("%s.%s.svc.cluster.local", name, s.Namespace)
		ip := s.Spec.ClusterIP
		// The mesh-Service ClusterIP is the A record for <svc>.<meshDomain>. Skip
		// headless/unallocated Services (no routable VIP to answer with).
		if ip != "" && ip != corev1.ClusterIPNone {
			records[svc] = ip
		}
		// EVERY mesh Service with a routable VIP is delivered (proposal 037
		// design (d)), not only the non-HTTP ones, because an HTTP-primary
		// service can still serve raw-TCP ports and those need chains too.
		//
		// PrimaryIsTCP is what the consumer keys the PORTLESS /32 floor chain
		// on. An HTTP-primary service must NOT get one: filter-chain match
		// precedence puts destination-IP above application-protocol, so a per-IP
		// chain would intercept HTTP to that VIP before the HCM chain could.
		// Its raw-TCP ports get destination_port-qualified chains instead, which
		// Envoy evaluates ahead of prefix_ranges and which therefore do not
		// disturb the VIP's HTTP traffic.
		//
		// The annotation is still the primary's protocol -- the registrar stamps
		// it from the primary port -- but it is no longer the gate on delivery.
		// Which PORTS are raw TCP is derived by the cache from the endpoints it
		// already holds, one hop closer to the source than this annotation, and
		// #878 is what happened when the two copies disagreed.
		if ip != "" && ip != corev1.ClusterIPNone {
			appProto := s.Annotations[aetherlabels.AnnotationMeshAppProtocol]
			tcpServices = append(tcpServices, CaptureTCPService{
				ServiceName: svc,
				ClusterIP:   ip,
				// Three-valued, not two: HTTP gets the HCM chain, UDP gets
				// neither chain (its path is the udp_proxy listener), and
				// everything else gets the portless TCP floor.
				PrimaryIsTCP: !isHTTPAppProtocol(appProto) && !isUDPAppProtocol(appProto),
			})
		}
	}

	r.Sink.SetCaptureAuthorities(authorities)
	r.Sink.SetMeshDNSRecords(records)
	r.Sink.SetCaptureTCPServices(tcpServices)
	r.Log.DebugContext(ctx, "projected mesh-Service authorities + DNS records", "meshServices", len(list.Items), "dnsRecords", len(records), "tcpServices", len(tcpServices))
	return reconcile.Result{}, nil
}
