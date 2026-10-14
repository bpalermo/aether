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
	"regexp"
	"slices"

	"aethermesh.dev/common/serviceref"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/ctrlqueue"
	commonlog "aethermesh.dev/common/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// CaptureTCPService describes a mesh Service with a routable address: the
// capture listener's raw-TCP chains match on that address (proposal 018,
// Phase 3a TCP floor; proposal 037). The ClusterIP drives the filter-chain
// prefix_ranges match; the cache derives the cluster name from ServiceName and
// its configured mesh domain, and the service's ports from the registry.
type CaptureTCPService struct {
	// ServiceName is the bare mesh service name (registry key). The cache uses this
	// to derive the TCP cluster name (TCPClusterName) and to look up SAN namespaces.
	ServiceName string
	// ClusterIP is the k8s Service ClusterIP the capture listener's filter chain
	// matches on (original-dst recovered via SO_ORIGINAL_DST). Must be a valid
	// non-None IP address (headless/unallocated Services are skipped).
	ClusterIP string
	// DeclaredHTTP and DeclaredOther are the REAL ports the generated Service
	// carries (its "p<port>-tcp" / "p<port>-udp" ports): the HTTP ones, and
	// the raw-TCP and UDP ones. The registrar derives them from the registry,
	// so they are what the registry will say; the agent has them from its
	// Service watch before, and whether or not, it holds an endpoint of the
	// service. The cache uses them for exactly that case: a service it holds
	// no endpoint of is routed and refused by its declared ports, like one it
	// knows, instead of by whatever the request's authority spells.
	DeclaredHTTP  []uint32
	DeclaredOther []uint32
}

// realPortName matches the name the registrar gives a real port of a generated
// mesh Service. Its fixed ports (mesh, mesh-tcp) are not ports of the service.
var realPortName = regexp.MustCompile(`^p[0-9]+-(tcp|udp)$`)

// declaredPorts reads a generated Service's real ports, sorted.
//
// A port number is in ONE of the two lists. The registrar writes one class per
// number, but the Service is an object anyone with the rights can edit, and a
// number carried as HTTP and as something else (p8080-tcp with appProtocol
// http beside p8080-udp) used to land in both: the route tables then claimed
// one authority in two virtual hosts, and the proxy rejects such a table
// WHOLE. HTTP wins, as it does when pods disagree. A number reserved for the
// mesh (mesh.ReservedPort) is no port of a service whatever the Service says:
// a registrar of chart 2.6 wrote a declared 18082 as a real port.
func declaredPorts(s *corev1.Service) (http, other []uint32) {
	defer func() {
		other = slices.DeleteFunc(other, func(p uint32) bool { return slices.Contains(http, p) })
	}()
	for _, sp := range s.Spec.Ports {
		if !realPortName.MatchString(sp.Name) || sp.Port <= 0 || mesh.ReservedPort(uint32(sp.Port)) {
			continue
		}
		tcp := sp.Protocol == "" || sp.Protocol == corev1.ProtocolTCP
		if tcp && sp.AppProtocol != nil && *sp.AppProtocol == "http" {
			http = append(http, uint32(sp.Port))
		} else {
			other = append(other, uint32(sp.Port))
		}
	}
	slices.Sort(http)
	slices.Sort(other)
	return slices.Compact(http), slices.Compact(other)
}

// AuthoritySink receives the projections from the generated mesh Services:
//   - SetCaptureAuthorities: service -> cluster.local FQDN (the cap_http route table, transparent capture).
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
		WatchesRawSource(source.Channel(initialProjection(), &handler.EnqueueRequestForObject{})).
		Named("capture").
		WithOptions(ctrlqueue.Options()).
		Complete(r)
}

// initialProjection returns a source holding exactly one event, so the
// reconciler projects once after its cache syncs even when there is no mesh
// Service to trigger it (#1094).
//
// The xDS server holds its first serve until the sink has seen one projection
// (cache.SnapshotCache.CaptureProjected), because the projection defines the
// capture listener's TCP chains. A node in a mesh with no Services would
// otherwise never project and would sit out the whole hold. Going through the
// controller's own workqueue, rather than calling Reconcile from a side
// goroutine, keeps every projection serialized: a second concurrent re-list
// could finish last with an older view.
//
// Reconcile ignores the request, so the object only has to be a valid key.
func initialProjection() <-chan event.GenericEvent {
	ch := make(chan event.GenericEvent, 1)
	ch <- event.GenericEvent{Object: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "aether-initial-projection"}}}
	return ch
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
		// design (d)): any of them can serve raw-TCP ports, which need chains
		// on its address.
		//
		// The name, the address and the real ports are read from the
		// Service. What its ports are and what speaks on each is derived by
		// the cache from the registry endpoints it holds, per port; the
		// Service's own ports stand in while the node holds none
		// (CaptureTCPService.DeclaredHTTP). The aether.io/app-protocol and aether.io/port annotations are
		// NOT read: they said one thing about the whole service, and #878 and
		// #1672 are what happened when that one thing was wrong. The registrar
		// no longer writes them, and removes them from the Services it owns.
		if ip != "" && ip != corev1.ClusterIPNone {
			http, other := declaredPorts(s)
			tcpServices = append(tcpServices, CaptureTCPService{ServiceName: svc, ClusterIP: ip, DeclaredHTTP: http, DeclaredOther: other})
		}
	}

	r.Sink.SetCaptureAuthorities(authorities)
	r.Sink.SetMeshDNSRecords(records)
	r.Sink.SetCaptureTCPServices(tcpServices)
	r.Log.DebugContext(ctx, "projected mesh-Service authorities + DNS records", "meshServices", len(list.Items), "dnsRecords", len(records), "tcpServices", len(tcpServices))
	return reconcile.Result{}, nil
}
