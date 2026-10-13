// Package services contains the registrar's mesh-Service generator: it projects the
// mesh service catalog into selectorless k8s Services — transparent-capture
// VIP/name handles (proposal 018, Phase 3a). Endpoints stay in the registry (the
// Services carry no selector, so no EndpointSlices). Each Service carries the
// service's real ports, each with its own protocol (proposal 043), beside the
// two mesh ports. A port's protocol is on the port; the Service carries no
// protocol or port for the service as a whole.
package services

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
// k8s Services (one per service, in the service's namespace). It is a
// leader-elected manager Runnable: only the leader writes Services.
type Generator struct {
	client.Client
	Snapshot *server.Snapshot
	MeshPort int32
	Interval time.Duration
	Log      *slog.Logger

	// classConflicts is the per-service port-class disagreement last warned
	// about, so a disagreement is logged once per distinct state and not once
	// per reconcile. Only the reconcile loop touches it.
	classConflicts map[client.ObjectKey]string
}

// sortEndpoints orders a service's rows by (cluster, address), in place.
//
// The snapshot hands a service's rows over in map order. Nothing the generator
// derives today depends on which row comes first (a port's class is a count,
// electClass), and a fixed order keeps it that way for whatever is added: when
// "the first endpoint" decided an annotation, the Service was rewritten every
// time the order fell the other way. The snapshot builds these slices for the
// caller, so sorting them in place touches nothing shared.
func sortEndpoints(eps []*registryv1.ServiceEndpoint) {
	slices.SortStableFunc(eps, endpointOrder)
}

// endpointOrder compares two rows by (cluster, address).
func endpointOrder(a, b *registryv1.ServiceEndpoint) int {
	if c := strings.Compare(a.GetClusterName(), b.GetClusterName()); c != 0 {
		return c
	}
	return strings.Compare(a.GetIp(), b.GetIp())
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
	// ports is the whole desired Spec.Ports: the mesh ports, then the
	// service's real ports.
	ports []corev1.ServicePort
}

// protocolAppProtocol names a port class: the appProtocol of a real TCP
// ServicePort (http or tcp), and the word a class conflict is logged with.
//
// Keep it total over registry.ServedProtocols
// (TestProtocolAppProtocolIsTotal): a class missing here would give its ports
// an empty appProtocol.
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

// buildDesiredServices builds the desired map of mesh VIP Services from every
// protocol listing in the snapshot.
//
// A service is one Service however many listings it is in. Its ports are the
// union of what its endpoints declare (servicePorts). A service with no
// namespaced endpoint in any listing gets no Service.
func (g *Generator) buildDesiredServices(ctx context.Context) map[client.ObjectKey]desiredService {
	// Every listing, read once, in registry.ServedProtocols' order (HTTP, TCP,
	// UDP).
	listings := make([]map[string][]*registryv1.ServiceEndpoint, len(registry.ServedProtocols))
	keys := map[string]struct{}{}
	for i, protocol := range registry.ServedProtocols {
		listings[i] = g.Snapshot.GetAll(protocol)
		for svcKey, eps := range listings[i] {
			keys[svcKey] = struct{}{}
			sortEndpoints(eps)
		}
	}

	desired := map[client.ObjectKey]desiredService{}
	conflicts := map[client.ObjectKey]string{}
	for svcKey := range keys {
		// The registry key is namespace-qualified "<ns>/<svc>" (020 Part 1):
		// the mesh VIP Service lives in <ns> with the bare ServiceAccount name.
		ref, ok := serviceref.ParseKey(svcKey)
		if !ok {
			g.Log.WarnContext(ctx, "skipping malformed (non-namespaced) service key", "key", svcKey)
			continue
		}
		// This service's rows, per listing, and nothing else: a port can only
		// come from an endpoint read under this key.
		perListing := make([][]*registryv1.ServiceEndpoint, len(listings))
		rows := 0
		for i := range listings {
			perListing[i] = namespaced(listings[i][svcKey])
			rows += len(perListing[i])
		}
		if rows == 0 {
			continue // no namespaced endpoint in any listing
		}
		realPorts, conflict := servicePorts(perListing)
		key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
		if conflict != "" {
			conflicts[key] = conflict
		}
		desired[key] = desiredService{
			service:   ref.Name,
			namespace: ref.Namespace,
			ports:     mergePorts(meshServicePorts(g.MeshPort), realPorts),
		}
	}
	g.warnClassConflicts(ctx, conflicts)
	return desired
}

// servicePorts returns the real ports of one service: one ServicePort per port
// number its endpoints declare, sorted by number (proposal 043).
//
// The set is the UNION over the service's endpoints across every listing, so a
// port appears with the first endpoint that declares it and leaves with the
// last. That is safe for the reason proposal 005 gave: a port's endpoint set is
// filtered per port, so a client of :9090 only reaches pods that declare 9090.
//
// What an endpoint declares is its port_protocols. A row with an empty map was
// written before the field existed: it declares its ports (or its one port)
// with the class of the listing it was read from.
//
// conflict describes the port numbers declared with more than one class, or is
// "" when there are none; see electClass.
func servicePorts(perListing [][]*registryv1.ServiceEndpoint) (ports []corev1.ServicePort, conflict string) {
	votes := portVotes{}
	for i, listing := range registry.ServedProtocols {
		for _, ep := range perListing[i] {
			// A pod that serves two classes is in two listings with the same
			// row, so a vote is one endpoint's, not one row's.
			voter := ep.GetClusterName() + "/" + ep.GetIp()
			for port, class := range declaredPorts(ep, listing) {
				votes.add(port, class, voter)
			}
		}
	}

	var conflicts []string
	for _, port := range slices.Sorted(maps.Keys(votes)) {
		counts := map[registryv1.Service_Protocol]int{}
		for class, endpoints := range votes[port] {
			counts[class] = len(endpoints)
		}
		class := electClass(counts)
		sp, ok := servicePort(port, class)
		if !ok {
			continue
		}
		ports = append(ports, sp)
		if len(counts) > 1 {
			conflicts = append(conflicts, describeConflict(port, counts, class))
		}
	}
	return ports, strings.Join(conflicts, "; ")
}

// portVotes is, for one service: port number -> class -> the endpoints that
// declare the port with that class.
type portVotes map[uint32]map[registryv1.Service_Protocol]map[string]struct{}

func (v portVotes) add(port uint32, class registryv1.Service_Protocol, voter string) {
	if port == 0 || port > 65535 {
		return // not a port a Service can carry
	}
	if v[port] == nil {
		v[port] = map[registryv1.Service_Protocol]map[string]struct{}{}
	}
	if v[port][class] == nil {
		v[port][class] = map[string]struct{}{}
	}
	v[port][class][voter] = struct{}{}
}

// declaredPorts returns what one row declares, port number -> class, when it
// was read from listing.
func declaredPorts(ep *registryv1.ServiceEndpoint, listing registryv1.Service_Protocol) map[uint32]registryv1.Service_Protocol {
	out := map[uint32]registryv1.Service_Protocol{}
	if len(ep.GetPortProtocols()) > 0 {
		for port, class := range ep.GetPortProtocols() {
			out[port] = listing
			if class != registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED {
				// The two enums are numerically identical (endpointmeta pins it).
				out[port] = registryv1.Service_Protocol(class)
			}
		}
		return out
	}
	// A row from before port_protocols existed: its served set, or its one
	// port, with the class of the listing.
	if len(ep.GetPorts()) == 0 {
		out[ep.GetPort()] = listing
	}
	for _, port := range ep.GetPorts() {
		out[port] = listing
	}
	return out
}

// electClass picks the one class a port number has when a service's endpoints
// declare it with several (a rollout that retypes a port). Capture can build
// one chain for a port number, so there has to be one answer: the class most
// endpoints declare, ties to HTTP, then TCP, then UDP. It reads only counts,
// so it does not depend on the order endpoints were listed in.
//
// No rule makes retyping a port hitless, because a client speaks one protocol.
// The supported way to change a port's protocol is a new port number.
//
// It answers for ONE port number and is private to this package on purpose: a
// service has no class.
func electClass(counts map[registryv1.Service_Protocol]int) registryv1.Service_Protocol {
	winner, best := registryv1.Service_PROTOCOL_UNSPECIFIED, 0
	// registry.ServedProtocols is HTTP, TCP, UDP: the tie order.
	for _, class := range registry.ServedProtocols {
		if counts[class] > best {
			winner, best = class, counts[class]
		}
	}
	return winner
}

func describeConflict(port uint32, counts map[registryv1.Service_Protocol]int, elected registryv1.Service_Protocol) string {
	parts := make([]string, 0, len(counts))
	for _, class := range registry.ServedProtocols {
		if n := counts[class]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", protocolAppProtocol[class], n))
		}
	}
	return fmt.Sprintf("port %d: %s, serving %s", port, strings.Join(parts, " "), protocolAppProtocol[elected])
}

// warnClassConflicts logs each service whose endpoints disagree about a port's
// class, once per distinct disagreement: again when the counts or the elected
// class change, and again if it comes back after being resolved.
func (g *Generator) warnClassConflicts(ctx context.Context, conflicts map[client.ObjectKey]string) {
	for key, conflict := range conflicts {
		if g.classConflicts[key] == conflict {
			continue
		}
		g.Log.WarnContext(ctx, "a mesh service's endpoints declare one port number with different protocols; "+
			"the Service carries the protocol most endpoints declare (to change a port's protocol, use a new port number)",
			"service", key.String(), "ports", conflict)
	}
	g.classConflicts = conflicts
}

// servicePort is the ServicePort of one real port: port = targetPort = the
// pod's port, named p<port>-<tcp|udp>. An HTTP or raw-TCP port is TCP at the
// Kubernetes level and says which in appProtocol; a UDP port has none.
func servicePort(port uint32, class registryv1.Service_Protocol) (corev1.ServicePort, bool) {
	p := int32(port) // 1..65535, checked where the port was declared
	sp := corev1.ServicePort{Port: p, TargetPort: intstr.FromInt32(p)}
	switch class {
	case registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP:
		sp.Protocol = corev1.ProtocolTCP
		sp.Name = fmt.Sprintf("p%d-tcp", port)
		sp.AppProtocol = new(protocolAppProtocol[class])
	case registryv1.Service_PROTOCOL_UDP:
		sp.Protocol = corev1.ProtocolUDP
		sp.Name = fmt.Sprintf("p%d-udp", port)
	default:
		return corev1.ServicePort{}, false
	}
	return sp, true
}

// mergePorts returns the Service's port list: the mesh ports, then the real
// ports in the order given. A real port with the port number and Kubernetes
// protocol of a mesh port REPLACES it, because the API server rejects two
// ports with the same number and protocol: a pod that declares 18082 as raw
// TCP yields p18082-tcp with appProtocol tcp and no "mesh-tcp".
func mergePorts(fixed, realPorts []corev1.ServicePort) []corev1.ServicePort {
	type slot struct {
		port     int32
		protocol corev1.Protocol
	}
	taken := make(map[slot]struct{}, len(realPorts))
	for _, p := range realPorts {
		taken[slot{p.Port, p.Protocol}] = struct{}{}
	}
	out := make([]corev1.ServicePort, 0, len(fixed)+len(realPorts))
	for _, p := range fixed {
		if _, replaced := taken[slot{p.Port, p.Protocol}]; !replaced {
			out = append(out, p)
		}
	}
	return append(out, realPorts...)
}

// pruneServices deletes managed mesh Services that are no longer in desired.
//
// A clusterset VIP Service (<svc>-clusterset) carries the mesh-service label
// too, so that agents capture its address, but the MCS import generator owns
// it and prunes it by its own label. It is not a registry service's name, so
// pruning it here deleted it on every reconcile, and the import generator
// created it again with a new ClusterIP.
func (g *Generator) pruneServices(ctx context.Context, managed corev1.ServiceList, desired map[client.ObjectKey]desiredService) {
	for i := range managed.Items {
		s := &managed.Items[i]
		key := client.ObjectKeyFromObject(s)
		if _, ok := desired[key]; ok {
			continue // converged below
		}
		if s.Labels[aetherlabels.LabelClustersetService] == "true" {
			continue // the MCS import generator's
		}
		if err := g.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			g.Log.ErrorContext(ctx, "prune mesh Service failed", "service", key.String(), "error", err)
		} else {
			g.Log.InfoContext(ctx, "pruned mesh Service (service gone from registry)", "service", key.String())
		}
	}
}

// meshServicePorts is the fixed port set every generated mesh Service exposes
// beside the service's real ports; mergePorts keeps each one unless a real
// port takes its number.
//
//   - "mesh" (18081) is the mesh's HTTP proxy port: a client sends
//     "Host: <name>:<port>" to "<name>:18081". It is not a port of the service.
//   - "mesh-tcp" (18082) is the raw-TCP and UDP spelling of the port the pods
//     declare.
//
// The capture listener matches both on destination_port, and the CNI diverts
// both for a pod that is not on redirect-all, so neither needs redirect-all.
// Both are TCP at the Kubernetes level and carry no appProtocol: they name a
// way in, not a port of the service.
//
// Until chart 2.7.0 there were two more, "http" (80) and "https" (443), so
// that a scheme-default dial had a Service port to land on. They are gone: a
// port the pods declare is on the Service as a real port, 80 and 443
// included, and a port they do not declare is not a port of the service.
func meshServicePorts(meshPort int32) []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "mesh", Port: meshPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(meshPort)},
		{Name: "mesh-tcp", Port: mesh.ProxyL4OutboundPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(mesh.ProxyL4OutboundPort)},
	}
}

// samePorts reports whether an existing Service already carries exactly want:
// the same names, each with the same port, protocol, targetPort and
// appProtocol. Order-insensitive: the API server may return them in any order
// and a reorder must not count as drift (#135 is the same mechanism on the
// route table).
//
// targetPort and appProtocol are compared because both carry meaning now: a
// real port's appProtocol is its class, and a Service left with the wrong one
// would tell every reader the wrong protocol for good.
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
		if !ok || g.Port != w.Port || g.Protocol != w.Protocol || g.TargetPort != w.TargetPort ||
			!sameAppProtocol(g.AppProtocol, w.AppProtocol) {
			return false
		}
	}
	return true
}

func sameAppProtocol(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// AppProtocolHTTP / AppProtocolTCP / AppProtocolUDP name the three port
// classes. AppProtocolHTTP and AppProtocolTCP are the two values a real TCP
// ServicePort's appProtocol takes; a UDP port has no appProtocol.
const (
	AppProtocolHTTP = "http"
	AppProtocolTCP  = "tcp"
	AppProtocolUDP  = "udp"
)

// staleServiceAnnotations are the two annotations the generator stamped on a
// mesh Service through chart 2.7.0: one protocol and one port for the whole
// service, read by a chart 2.6 agent to choose a capture chain. Nothing reads
// them any more (a port's protocol is on the ServicePort and in the registry),
// so the generator no longer writes them and removes them from the Services it
// owns.
var staleServiceAnnotations = [...]string{aetherlabels.AnnotationMeshPort, aetherlabels.AnnotationMeshAppProtocol}

// dropStaleAnnotations removes staleServiceAnnotations from annotations and
// reports whether there was one to remove.
func dropStaleAnnotations(annotations map[string]string) bool {
	dropped := false
	for _, a := range staleServiceAnnotations {
		if _, ok := annotations[a]; ok {
			delete(annotations, a)
			dropped = true
		}
	}
	return dropped
}

// apply creates or updates the VIP Service for one mesh service. It NEVER touches a
// Service it doesn't own — a name collision with a user's Service is logged, not
// clobbered.
func (g *Generator) apply(ctx context.Context, d desiredService) {
	key := client.ObjectKey{Namespace: d.namespace, Name: d.service}

	existing := &corev1.Service{}
	if err := g.Get(ctx, key, existing); err == nil {
		g.converge(ctx, key, existing, d)
		return
	} else if !apierrors.IsNotFound(err) {
		g.Log.ErrorContext(ctx, "get mesh Service failed", "service", key.String(), "error", err)
		return
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        d.service,
			Namespace:   d.namespace,
			Labels:      map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{aetherlabels.AnnotationMeshService: d.service},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Selectorless: a pure VIP + cluster.local name handle. Endpoints stay in
			// the aether registry; the agent maps this ClusterIP -> the EDS cluster.
			Ports: d.ports,
		},
	}
	if err := g.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		g.Log.ErrorContext(ctx, "create mesh Service failed", "service", key.String(), "error", err)
	} else if err == nil {
		g.Log.InfoContext(ctx, "created mesh Service (transparent-capture VIP)", "service", key.String())
	}
}

// converge brings an EXISTING Service of the mesh service's name to d: its
// ports, and the two stale annotations gone. One Update when either differs,
// none when the Service is already right. A Service the generator does not own
// is left exactly as it is.
func (g *Generator) converge(ctx context.Context, key client.ObjectKey, existing *corev1.Service, d desiredService) {
	if existing.Labels[aetherlabels.LabelMeshService] != "true" {
		g.Log.WarnContext(ctx, "a non-aether Service owns this name; skipping mesh VIP", "service", key.String())
		return
	}
	// Spec.Ports is part of what must converge. This path once compared
	// annotations alone and returned early, so a Service created before
	// proposal 037 would have kept its single "mesh" port forever.
	cleaned := dropStaleAnnotations(existing.Annotations)
	if !cleaned && samePorts(existing.Spec.Ports, d.ports) {
		return // converged
	}
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[aetherlabels.AnnotationMeshService] = d.service
	existing.Spec.Ports = d.ports
	if err := g.Update(ctx, existing); err != nil {
		g.Log.ErrorContext(ctx, "update mesh Service failed", "service", key.String(), "error", err)
	}
}

// namespaced returns the endpoints that carry a Kubernetes namespace, in order.
// An endpoint without one contributes no port to a generated Service, and a
// service with no namespaced endpoint gets none.
func namespaced(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
	out := make([]*registryv1.ServiceEndpoint, 0, len(eps))
	for _, ep := range eps {
		if ep.GetKubernetesMetadata().GetNamespace() != "" {
			out = append(out, ep)
		}
	}
	return out
}
