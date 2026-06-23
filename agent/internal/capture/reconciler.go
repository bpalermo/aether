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

	"github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// AuthoritySink receives the projections from the generated mesh Services:
//   - SetCaptureAuthorities: service -> cluster.local FQDN (cap_http, transparent capture).
//   - SetMeshDNSRecords:     service -> mesh-Service ClusterIP (the per-pod dns_filter's
//     A record for <svc>.<meshDomain>, the mesh-global FQDN).
//
// Both are emitted every reconcile; the cache uses whichever feature is enabled.
type AuthoritySink interface {
	SetCaptureAuthorities(authorities map[string]string)
	SetMeshDNSRecords(records map[string]string)
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

// Reconcile re-lists the mesh Services and projects their cluster.local authorities.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, client.MatchingLabels{constants.LabelMeshService: "true"}); err != nil {
		return reconcile.Result{}, err
	}

	authorities := make(map[string]string, len(list.Items))
	records := make(map[string]string, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		svc := s.Annotations[constants.AnnotationMeshService]
		if svc == "" {
			svc = s.Name
		}
		authorities[svc] = fmt.Sprintf("%s.%s.svc.cluster.local", s.Name, s.Namespace)
		// The mesh-Service ClusterIP is the A record for <svc>.<meshDomain>. Skip
		// headless/unallocated Services (no routable VIP to answer with).
		if ip := s.Spec.ClusterIP; ip != "" && ip != corev1.ClusterIPNone {
			records[svc] = ip
		}
	}

	r.Sink.SetCaptureAuthorities(authorities)
	r.Sink.SetMeshDNSRecords(records)
	r.Log.DebugContext(ctx, "projected mesh-Service authorities + DNS records", "meshServices", len(list.Items), "dnsRecords", len(records))
	return reconcile.Result{}, nil
}
