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
	"github.com/bpalermo/aether/common/serviceref"
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
	service     string
	namespace   string
	port        uint32
	appProtocol string // "http" (PROTOCOL_HTTP) or "tcp" (PROTOCOL_TCP)
}

// protocolAppProtocol maps a registry service protocol to the AnnotationMeshAppProtocol
// value the generator stamps on the mesh Service (the agent's capture reconciler reads
// it to decide HCM vs. TCP-floor chain emission).
var protocolAppProtocol = map[registryv1.Service_Protocol]string{
	registryv1.Service_PROTOCOL_HTTP: AppProtocolHTTP,
	registryv1.Service_PROTOCOL_TCP:  AppProtocolTCP,
}

// reconcile makes the managed Services equal the snapshot catalog: create missing,
// update drifted, prune stale (only Services this generator owns, by label).
func (g *Generator) reconcile(ctx context.Context) {
	desired := map[client.ObjectKey]desiredService{}
	// A service is registered under exactly one protocol (the registry key is
	// name+protocol; the pod annotation picks it). Iterate every protocol so an
	// HTTP service gets an "http" mesh Service and a TCP service a "tcp" one.
	// Iteration is ordered (HTTP before TCP) so the no-clobber convergence is
	// deterministic if a name ever appeared under both.
	for _, protocol := range []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP} {
		appProto := protocolAppProtocol[protocol]
		for svcKey, eps := range g.Snapshot.GetAll(protocol) {
			// The registry key is namespace-qualified "<ns>/<svc>" (020 Part 1):
			// the mesh VIP Service lives in <ns> with the bare ServiceAccount name.
			ref, ok := serviceref.ParseKey(svcKey)
			if !ok {
				g.Log.WarnContext(ctx, "skipping malformed (non-namespaced) service key", "key", svcKey)
				continue
			}
			ep := firstNamespaced(eps)
			if ep == nil {
				continue
			}
			key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
			if _, exists := desired[key]; exists {
				continue // already claimed by an earlier protocol; one Service per name
			}
			desired[key] = desiredService{
				service:     ref.Name,
				namespace:   ref.Namespace,
				port:        ep.GetPort(),
				appProtocol: appProto,
			}
		}
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

// AppProtocolHTTP / AppProtocolTCP are the AnnotationMeshAppProtocol values the
// generator writes (and the agent reads to decide TCP-floor chain emission).
const (
	AppProtocolHTTP = "http"
	AppProtocolTCP  = "tcp"
)

// apply creates or updates the VIP Service for one mesh service. It NEVER touches a
// Service it doesn't own — a name collision with a user's Service is logged, not
// clobbered.
func (g *Generator) apply(ctx context.Context, d desiredService) {
	key := client.ObjectKey{Namespace: d.namespace, Name: d.service}
	port := strconv.Itoa(int(d.port))
	appProto := d.appProtocol
	if appProto == "" {
		appProto = AppProtocolHTTP
	}

	existing := &corev1.Service{}
	if err := g.Get(ctx, key, existing); err == nil {
		if existing.Labels[constants.LabelMeshService] != "true" {
			g.Log.WarnContext(ctx, "a non-aether Service owns this name; skipping mesh VIP", "service", key.String())
			return
		}
		if existing.Annotations[constants.AnnotationMeshPort] == port &&
			existing.Annotations[constants.AnnotationMeshAppProtocol] == appProto {
			return // converged
		}
		existing.Annotations[constants.AnnotationMeshService] = d.service
		existing.Annotations[constants.AnnotationMeshPort] = port
		existing.Annotations[constants.AnnotationMeshAppProtocol] = appProto
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
				constants.AnnotationMeshService:     d.service,
				constants.AnnotationMeshPort:        port,
				constants.AnnotationMeshAppProtocol: appProto,
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
