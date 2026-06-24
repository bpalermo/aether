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

	"github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
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
		return o.GetLabels()[constants.LabelMeshService] == "true"
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

// Reconcile re-lists the mesh Services and projects their cluster.local authorities,
// DNS records, and TCP-floor service set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, client.MatchingLabels{constants.LabelMeshService: "true"}); err != nil {
		return reconcile.Result{}, err
	}

	authorities := make(map[string]string, len(list.Items))
	records := make(map[string]string, len(list.Items))
	var tcpServices []CaptureTCPService
	for i := range list.Items {
		s := &list.Items[i]
		svc := s.Annotations[constants.AnnotationMeshService]
		if svc == "" {
			svc = s.Name
		}
		authorities[svc] = fmt.Sprintf("%s.%s.svc.cluster.local", s.Name, s.Namespace)
		ip := s.Spec.ClusterIP
		// The mesh-Service ClusterIP is the A record for <svc>.<meshDomain>. Skip
		// headless/unallocated Services (no routable VIP to answer with).
		if ip != "" && ip != corev1.ClusterIPNone {
			records[svc] = ip
		}
		// Non-HTTP services need a per-ClusterIP TCP-proxy floor chain on the
		// capture listener. HTTP services are handled by the global HCM chain and
		// do not need — and must not have — a per-IP chain (filter-chain match
		// precedence: destination-IP wins over application-protocol, so a per-IP
		// chain would intercept HTTP to that VIP before the HCM chain could).
		appProto := s.Annotations[constants.AnnotationMeshAppProtocol]
		if !isHTTPAppProtocol(appProto) && ip != "" && ip != corev1.ClusterIPNone {
			tcpServices = append(tcpServices, CaptureTCPService{
				ServiceName: svc,
				ClusterIP:   ip,
			})
		}
	}

	r.Sink.SetCaptureAuthorities(authorities)
	r.Sink.SetMeshDNSRecords(records)
	r.Sink.SetCaptureTCPServices(tcpServices)
	r.Log.DebugContext(ctx, "projected mesh-Service authorities + DNS records", "meshServices", len(list.Items), "dnsRecords", len(records), "tcpServices", len(tcpServices))
	return reconcile.Result{}, nil
}
