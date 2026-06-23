// Package services contains the registrar's mesh-Service generator: it projects the
// mesh service catalog into selectorless k8s Services on the mesh port — transparent-
// capture VIP/name handles (proposal 018, Phase 3a). Endpoints stay in the registry
// (the Services carry no selector, so no EndpointSlices); each Service is annotated
// with the mesh service + app port so the agent can map a captured ClusterIP
// (original-dst) back to the registry-backed EDS cluster.
package services

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registrar/internal/server"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Generator reconciles the registrar snapshot's mesh services into selectorless
// k8s Services (one per service, in the service's namespace, on MeshPort). It is a
// leader-elected manager Runnable: only the leader writes Services.
type Generator struct {
	client.Client
	Snapshot *server.Snapshot
	MeshPort int32
	Interval time.Duration
	Log      *slog.Logger
}

// NeedLeaderElection makes the generator run only on the elected leader (single writer).
func (g *Generator) NeedLeaderElection() bool { return true }

// Start runs the reconcile loop until the context is cancelled.
func (g *Generator) Start(ctx context.Context) error {
	g.Log = commonlog.Named(g.Log, "service-generator")
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	g.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			g.reconcile(ctx)
		}
	}
}

type desiredService struct {
	service   string
	namespace string
	port      uint32
}

// reconcile makes the managed Services equal the snapshot catalog: create missing,
// update drifted, prune stale (only Services this generator owns, by label).
func (g *Generator) reconcile(ctx context.Context) {
	desired := map[client.ObjectKey]desiredService{}
	for svc, eps := range g.Snapshot.GetAll(registryv1.Service_PROTOCOL_HTTP) {
		ep := firstNamespaced(eps)
		if ep == nil {
			continue
		}
		ns := ep.GetKubernetesMetadata().GetNamespace()
		desired[client.ObjectKey{Namespace: ns, Name: svc}] = desiredService{service: svc, namespace: ns, port: ep.GetPort()}
	}

	var managed corev1.ServiceList
	if err := g.List(ctx, &managed, client.MatchingLabels{constants.LabelMeshService: "true"}); err != nil {
		g.Log.ErrorContext(ctx, "list managed mesh Services failed", "error", err)
		return
	}
	for i := range managed.Items {
		s := &managed.Items[i]
		key := client.ObjectKeyFromObject(s)
		if _, ok := desired[key]; ok {
			continue // converged below
		}
		if err := g.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			g.Log.ErrorContext(ctx, "prune mesh Service failed", "service", key.String(), "error", err)
		} else {
			g.Log.InfoContext(ctx, "pruned mesh Service (service gone from registry)", "service", key.String())
		}
	}
	for _, d := range desired {
		g.apply(ctx, d)
	}
}

// apply creates or updates the VIP Service for one mesh service. It NEVER touches a
// Service it doesn't own — a name collision with a user's Service is logged, not
// clobbered.
func (g *Generator) apply(ctx context.Context, d desiredService) {
	key := client.ObjectKey{Namespace: d.namespace, Name: d.service}
	port := strconv.Itoa(int(d.port))

	existing := &corev1.Service{}
	if err := g.Get(ctx, key, existing); err == nil {
		if existing.Labels[constants.LabelMeshService] != "true" {
			g.Log.WarnContext(ctx, "a non-aether Service owns this name; skipping mesh VIP", "service", key.String())
			return
		}
		if existing.Annotations[constants.AnnotationMeshPort] == port {
			return // converged
		}
		existing.Annotations[constants.AnnotationMeshService] = d.service
		existing.Annotations[constants.AnnotationMeshPort] = port
		if err := g.Update(ctx, existing); err != nil {
			g.Log.ErrorContext(ctx, "update mesh Service failed", "service", key.String(), "error", err)
		}
		return
	} else if !apierrors.IsNotFound(err) {
		g.Log.ErrorContext(ctx, "get mesh Service failed", "service", key.String(), "error", err)
		return
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.service,
			Namespace: d.namespace,
			Labels:    map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{
				constants.AnnotationMeshService: d.service,
				constants.AnnotationMeshPort:    port,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Selectorless: a pure VIP + cluster.local name handle. Endpoints stay in
			// the aether registry; the agent maps this ClusterIP -> the EDS cluster.
			Ports: []corev1.ServicePort{{
				Name:       "mesh",
				Port:       g.MeshPort,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(g.MeshPort),
			}},
		},
	}
	if err := g.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		g.Log.ErrorContext(ctx, "create mesh Service failed", "service", key.String(), "error", err)
	} else if err == nil {
		g.Log.InfoContext(ctx, "created mesh Service (transparent-capture VIP)", "service", key.String(), "port", port)
	}
}

func firstNamespaced(eps []*registryv1.ServiceEndpoint) *registryv1.ServiceEndpoint {
	for _, ep := range eps {
		if ep.GetKubernetesMetadata().GetNamespace() != "" {
			return ep
		}
	}
	return nil
}
