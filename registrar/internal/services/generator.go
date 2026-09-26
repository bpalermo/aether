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

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/constants/mesh"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/registrar/internal/server"
	"aethermesh.dev/registry"
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
	appProtocol string // "http", "tcp" or "udp" -- see protocolAppProtocol
}

// protocolAppProtocol maps a registry service protocol to the AnnotationMeshAppProtocol
// value the generator stamps on the mesh Service (the agent's capture reconciler reads
// it to decide HCM vs. TCP-floor vs. UDP-floor chain emission).
//
// A protocol missing from this map yields "", which apply() coerces to
// AppProtocolHTTP -- so an omission here does not fail, it mislabels the service
// as HTTP and sends its traffic down the HCM path. Keep it total over
// registry.ServedProtocols; TestProtocolAppProtocolIsTotal enforces that.
var protocolAppProtocol = map[registryv1.Service_Protocol]string{
	registryv1.Service_PROTOCOL_HTTP: AppProtocolHTTP,
	registryv1.Service_PROTOCOL_TCP:  AppProtocolTCP,
	registryv1.Service_PROTOCOL_UDP:  AppProtocolUDP,
}

// reconcile makes the managed Services equal the snapshot catalog: create missing,
// update drifted, prune stale (only Services this generator owns, by label).
func (g *Generator) reconcile(ctx context.Context) {
	desired := g.buildDesiredServices(ctx)

	var managed corev1.ServiceList
	if err := g.List(ctx, &managed, client.MatchingLabels{aetherlabels.LabelMeshService: "true"}); err != nil {
		g.Log.ErrorContext(ctx, "list managed mesh Services failed", "error", err)
		return
	}
	g.pruneServices(ctx, managed, desired)
	for _, d := range desired {
		g.apply(ctx, d)
	}
}

// buildDesiredServices iterates all protocols in the snapshot and builds the
// desired map of mesh VIP Services. Ordered (registry.ServedProtocols) for
// determinism.
func (g *Generator) buildDesiredServices(ctx context.Context) map[client.ObjectKey]desiredService {
	desired := map[client.ObjectKey]desiredService{}
	// A service may be registered under MORE than one protocol since proposal
	// 037: a pod advertising an HTTP port and a raw TCP port is written under
	// both keys. Iterate every protocol so each service gets a mesh Service
	// labelled with the app-protocol its primary port speaks.
	//
	// Iteration order is registry.ServedProtocols' order (HTTP, TCP, UDP), which
	// makes the no-clobber convergence below deterministic when a name appears
	// under several: the first protocol iterated wins.
	for _, protocol := range registry.ServedProtocols {
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
	return desired
}

// pruneServices deletes managed mesh Services that are no longer in desired.
func (g *Generator) pruneServices(ctx context.Context, managed corev1.ServiceList, desired map[client.ObjectKey]desiredService) {
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
}

// meshServicePorts is the port set every generated mesh Service exposes
// (proposal 037).
//
//   - "mesh" (18081) and "mesh-tcp" (18082) are the mesh's two well-known
//     spellings: HTTP and raw TCP respectively. The capture listener matches
//     both on destination_port, and the scoped CNI rule redirects both, so
//     neither needs redirect-all.
//   - "http" (80) and "https" (443) exist so a scheme-default dial has a REAL
//     Service port to land on. These Services are selectorless and hold no
//     endpoints, so kube-proxy REJECTs a connection to an unclaimed port: an
//     uncaptured `https://<svc>/` fails immediately with ECONNREFUSED instead
//     of hanging in a CNI-dependent way. They carry no data path of their own.
//
// All four are TCP at the Kubernetes level; the mesh protocol is a separate
// axis carried by the aether.io/app-protocol annotation.
func meshServicePorts(meshPort int32) []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "mesh", Port: meshPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(meshPort)},
		{Name: "mesh-tcp", Port: mesh.ProxyL4OutboundPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(mesh.ProxyL4OutboundPort)},
		{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(80)},
		{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(443)},
	}
}

// samePorts reports whether an existing Service already carries exactly want,
// comparing name/port/protocol. Order-insensitive: the API server may return
// them in any order and a reorder must not count as drift (#135 is the same
// mechanism on the route table).
func samePorts(got, want []corev1.ServicePort) bool {
	if len(got) != len(want) {
		return false
	}
	index := make(map[string]corev1.ServicePort, len(got))
	for _, p := range got {
		index[p.Name] = p
	}
	for _, w := range want {
		g, ok := index[w.Name]
		if !ok || g.Port != w.Port || g.Protocol != w.Protocol {
			return false
		}
	}
	return true
}

// AppProtocolHTTP / AppProtocolTCP / AppProtocolUDP are the
// AnnotationMeshAppProtocol values the generator writes (and the agent reads to
// decide HCM vs. TCP-floor vs. UDP-floor chain emission).
const (
	AppProtocolHTTP = "http"
	AppProtocolTCP  = "tcp"
	AppProtocolUDP  = "udp"
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
		if existing.Labels[aetherlabels.LabelMeshService] != "true" {
			g.Log.WarnContext(ctx, "a non-aether Service owns this name; skipping mesh VIP", "service", key.String())
			return
		}
		// Spec.Ports is part of what must converge, not just the annotations.
		// This path used to compare annotations alone and return early, so a
		// Service created before proposal 037 would have kept its single "mesh"
		// port forever and the new spellings would never have appeared on an
		// existing cluster — the feature would have worked only on a fresh
		// install, which is the worst way to find out.
		wantPorts := meshServicePorts(g.MeshPort)
		if existing.Annotations[aetherlabels.AnnotationMeshPort] == port &&
			existing.Annotations[aetherlabels.AnnotationMeshAppProtocol] == appProto &&
			samePorts(existing.Spec.Ports, wantPorts) {
			return // converged
		}
		existing.Annotations[aetherlabels.AnnotationMeshService] = d.service
		existing.Annotations[aetherlabels.AnnotationMeshPort] = port
		existing.Annotations[aetherlabels.AnnotationMeshAppProtocol] = appProto
		existing.Spec.Ports = wantPorts
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
			Labels:    map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     d.service,
				aetherlabels.AnnotationMeshPort:        port,
				aetherlabels.AnnotationMeshAppProtocol: appProto,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Selectorless: a pure VIP + cluster.local name handle. Endpoints stay in
			// the aether registry; the agent maps this ClusterIP -> the EDS cluster.
			Ports: meshServicePorts(g.MeshPort),
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
