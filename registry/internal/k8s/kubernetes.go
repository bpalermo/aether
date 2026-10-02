// Package k8s implements the Registry interface using the Kubernetes API server as the backend.
// It discovers service endpoints by listing pods with the aether.io/managed=true label
// and derives service names from each pod's ServiceAccount. Node topology labels provide
// region and zone locality information.
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	aetherlabels "aethermesh.dev/common/constants/labels"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/registry/endpointmeta"
)

// Config holds the configuration for the Kubernetes registry backend.
type Config struct {
	// ClusterName is the name of the cluster, used to populate ServiceEndpoint.ClusterName.
	ClusterName string
	// Informers, when set, is where the backend gets a shared Pod informer to
	// push change notifications from (registry.ChangeNotifier): the
	// controller-runtime manager's cache in the registrar. Nil leaves the
	// backend poll-only.
	Informers PodInformerSource
}

// PodInformerSource yields a shared Pod informer. The controller-runtime cache
// (ctrlcache.Informers) satisfies it; the narrow interface is what lets tests
// drive the handler with a fake informer.
type PodInformerSource interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error)
}

// nodeLocalityCacheTTL bounds how long cached node localities are served without
// re-listing nodes. Topology labels essentially never change and a node's
// InternalIP is stable for its lifetime, so a few minutes of staleness is safe;
// a pod landing on a node that is not yet cached forces an immediate refresh
// regardless of the TTL (see buildNodeLocalities).
const nodeLocalityCacheTTL = 5 * time.Minute

// KubernetesRegistry is a Registry implementation backed by the Kubernetes API server.
// It reads pods labeled with aether.io/managed=true and converts them to ServiceEndpoints.
// Write operations (Register/Unregister) are no-ops since the API server is the source of truth.
//
// Drain state lives on the Pod too (aether#1124): a pod whose deletion was
// requested lists as DRAINING (see podHealth). Because writes are no-ops, the
// DRAINING mark an agent sends lands only in the snapshot of the registrar
// replica that received it; every OTHER replica learns the drain from the same
// deletionTimestamp the agent reacted to and, with Informers set, at watch
// speed rather than at its next poll.
type KubernetesRegistry struct {
	log         *slog.Logger
	clusterName string
	reader      client.Reader

	// informers feeds notify (registry.ChangeNotifier); nil = poll-only.
	informers PodInformerSource
	notify    chan struct{}

	// Node-locality cache (issue #541): List/ListAll run on every registry
	// reload — dozens of times per minute during churn — and used to resolve
	// node localities with serial per-node Gets each call. The cache is
	// refreshed with a single node List and then served from memory until the
	// TTL lapses or an unknown node shows up.
	nodeMu          sync.Mutex
	nodeCache       map[string]locality
	nodeCacheExpiry time.Time
	nodeCacheTTL    time.Duration
	now             func() time.Time
}

// NewKubernetesRegistry creates a new Kubernetes API server backed Registry.
// The reader should be a direct API reader (e.g., manager.GetAPIReader()) to avoid
// cache synchronization issues during startup.
func NewKubernetesRegistry(log *slog.Logger, reader client.Reader, cfg Config) *KubernetesRegistry {
	r := &KubernetesRegistry{
		log:          commonlog.Named(log, "registry-kubernetes"),
		clusterName:  cfg.ClusterName,
		reader:       reader,
		informers:    cfg.Informers,
		nodeCacheTTL: nodeLocalityCacheTTL,
		now:          time.Now,
	}
	if cfg.Informers != nil {
		r.notify = make(chan struct{}, 1)
	}
	return r
}

// Initialize registers the Pod change handler when an informer source is
// configured. The API server connection is managed by the controller-runtime
// manager; the informer starts with the manager's cache, so this does not
// block on its sync.
func (r *KubernetesRegistry) Initialize(ctx context.Context) error {
	if r.informers != nil {
		informer, err := r.informers.GetInformer(ctx, &corev1.Pod{}, ctrlcache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("failed to get pod informer: %w", err)
		}
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    r.onPodAddOrDelete,
			UpdateFunc: r.onPodUpdate,
			DeleteFunc: r.onPodAddOrDelete,
		}); err != nil {
			return fmt.Errorf("failed to add pod event handler: %w", err)
		}
	}
	r.log.Info("kubernetes registry initialized", "cluster", r.clusterName, "podWatch", r.informers != nil)
	return nil
}

func (r *KubernetesRegistry) onPodAddOrDelete(obj any) {
	if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if pod, ok := obj.(*corev1.Pod); ok && isManaged(pod) {
		r.signal()
	}
}

func (r *KubernetesRegistry) onPodUpdate(oldObj, newObj any) {
	oldPod, okOld := oldObj.(*corev1.Pod)
	newPod, okNew := newObj.(*corev1.Pod)
	if okOld && okNew && (isManaged(oldPod) || isManaged(newPod)) && endpointInputsChanged(oldPod, newPod) {
		r.signal()
	}
}

// Changes implements registry.ChangeNotifier: a coalesced signal whenever a
// managed pod changes in a way that can change its endpoint. Nil (never fires)
// when the backend was built without an informer source.
func (r *KubernetesRegistry) Changes() <-chan struct{} { return r.notify }

func (r *KubernetesRegistry) signal() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func isManaged(pod *corev1.Pod) bool {
	return pod.Labels[aetherlabels.LabelAetherManaged] == "true"
}

// endpointInputsChanged reports whether an update touched anything the
// listing or podToEndpoint reads, so status-only churn (restart counts, probe
// timestamps) does not trigger a full re-list.
func endpointInputsChanged(oldPod, newPod *corev1.Pod) bool {
	return isManaged(oldPod) != isManaged(newPod) ||
		(oldPod.DeletionTimestamp == nil) != (newPod.DeletionTimestamp == nil) ||
		oldPod.Status.Phase != newPod.Status.Phase ||
		oldPod.Status.PodIP != newPod.Status.PodIP ||
		podReady(oldPod) != podReady(newPod) ||
		oldPod.Spec.NodeName != newPod.Spec.NodeName ||
		oldPod.Spec.ServiceAccountName != newPod.Spec.ServiceAccountName ||
		!maps.Equal(oldPod.Annotations, newPod.Annotations)
}

// Close is a no-op for the Kubernetes registry.
func (r *KubernetesRegistry) Close() error {
	return nil
}

// RegisterEndpoint is a no-op. The Kubernetes API server is the source of truth for pod endpoints.
func (r *KubernetesRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	return nil
}

// UnregisterEndpoint is a no-op. The Kubernetes API server is the source of truth for pod endpoints.
func (r *KubernetesRegistry) UnregisterEndpoint(_ context.Context, _ string, _ string) error {
	return nil
}

// UnregisterEndpoints is a no-op. The Kubernetes API server is the source of truth for pod endpoints.
func (r *KubernetesRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	return nil
}

// ListEndpoints returns all endpoints for a service by listing managed pods whose ServiceAccount
// matches the given service name. Node topology labels are used for locality information.
//
// Endpoints are filtered by the protocol each POD declares in
// endpoint.aether.io/protocol, so a query for P returns exactly the pods serving P
// (#878).
//
// This backend used to return nothing at all for a TCP query. That was a workaround,
// not a property of Kubernetes: podToEndpoint ignored the protocol annotation, so
// every managed pod looked like an HTTP endpoint, and answering a TCP query with
// that same set made every mesh service collapse to a TCP-only entry — the agent's
// LoadClustersFromRegistry builds a service's HTTP cluster (with its outbound and
// cap_http vhosts) from the HTTP listing, then OVERWRITES the same map key with a
// vhost-less tcp:true entry from the TCP listing. The CDS cluster and GAMMA vhost
// vanished and captured requests 503'd with no_healthy_upstream.
//
// Reading the annotation removes the need for the workaround: a pod declaring "tcp"
// appears only in the TCP listing and a pod declaring "http" only in the HTTP one,
// so the two listings are disjoint and the agent's second pass has nothing to
// clobber. The "a service is HTTP or TCP, never both" invariant still holds, because
// every pod behind one ServiceAccount carries the same annotation.
//
// Filtering runs in BOTH directions on purpose. Returning HTTP-declaring pods under
// TCP is the bug above; returning TCP-declaring pods under HTTP is the same bug
// mirrored, and would put a raw-TCP pod behind an h2 cluster.
func (r *KubernetesRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing endpoints", "service", service)

	pods, err := r.listManagedPods(ctx)
	if err != nil {
		return nil, err
	}

	nodeLocalities, err := r.buildNodeLocalities(ctx, pods)
	if err != nil {
		return nil, err
	}

	var endpoints []*registryv1.ServiceEndpoint
	for i := range pods {
		pod := &pods[i]
		// The ServiceAccount filter stays in memory: spec.serviceAccountName is
		// NOT a supported server-side pod field selector, and managed pods carry
		// no per-service label — only aether.io/managed=true (issue #541).
		//
		// The registry key is the namespace-qualified "<ns>/<sa>" (020 Part 1),
		// matching the CNI registration path (registry/cni.go) and the etcd/ddb
		// backends. Keying by the bare ServiceAccount would collide same-named SAs
		// across namespaces (e.g. echo-v1 in two namespaces).
		if pod.Spec.ServiceAccountName == "" {
			continue
		}
		if serviceref.New(pod.Namespace, pod.Spec.ServiceAccountName).Key() != service {
			continue
		}
		if !r.podServesProtocol(ctx, pod, protocol) {
			continue
		}
		ep, err := r.podToEndpoint(pod, nodeLocalities)
		if err != nil {
			r.log.ErrorContext(ctx, "failed to convert pod to endpoint", "error", err, "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		endpoints = append(endpoints, ep)
	}

	r.log.DebugContext(ctx, "listed endpoints", "service", service, "count", len(endpoints))
	return endpoints, nil
}

// ListAllEndpoints returns all endpoints for all services, grouped by service name (ServiceAccount).
// It lists all managed pods, resolves node localities, and converts each pod to a ServiceEndpoint.
//
// Pods are filtered by the protocol each declares in endpoint.aether.io/protocol,
// exactly as in ListEndpoints — see there for why this backend used to return an
// empty map for TCP and why it no longer has to (#878). A service with no pod
// serving the requested protocol is absent from the result rather than present and
// empty, so the agent's cluster passes see only the services they should build.
func (r *KubernetesRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing all endpoints")

	pods, err := r.listManagedPods(ctx)
	if err != nil {
		return nil, err
	}

	nodeLocalities, err := r.buildNodeLocalities(ctx, pods)
	if err != nil {
		return nil, err
	}

	endpointsByService := make(map[string][]*registryv1.ServiceEndpoint)
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.ServiceAccountName == "" {
			r.log.DebugContext(ctx, "skipping pod without service account", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		// Namespace-qualified "<ns>/<sa>" key (020 Part 1), matching the CNI
		// registration path and the etcd/ddb backends. Bare-SA keying collided
		// same-named ServiceAccounts across namespaces — e.g. echo-v1 in two
		// conformance namespaces merged into one entry, whose endpoint set then
		// oscillated and churned the agent's xDS snapshot.
		serviceName := serviceref.New(pod.Namespace, pod.Spec.ServiceAccountName).Key()

		if !r.podServesProtocol(ctx, pod, protocol) {
			continue
		}

		ep, err := r.podToEndpoint(pod, nodeLocalities)
		if err != nil {
			r.log.ErrorContext(ctx, "failed to convert pod to endpoint", "error", err, "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		endpointsByService[serviceName] = append(endpointsByService[serviceName], ep)
	}

	r.log.DebugContext(ctx, "listed all endpoints", "services", len(endpointsByService))
	return endpointsByService, nil
}

// locality holds the region and zone for a node.
type locality struct {
	region string
	zone   string
	nodeIP string
}

// listManagedPods lists all running pods with the aether.io/managed=true label that have a PodIP.
func (r *KubernetesRegistry) listManagedPods(ctx context.Context) ([]corev1.Pod, error) {
	var podList corev1.PodList
	err := r.reader.List(
		ctx, &podList,
		client.MatchingLabels{aetherlabels.LabelAetherManaged: "true"},
	)
	if err != nil {
		r.log.ErrorContext(ctx, "failed to list managed pods", "error", err)
		return nil, fmt.Errorf("failed to list managed pods: %w", err)
	}

	// Filter to running pods with an IP
	var pods []corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			pods = append(pods, *pod)
		}
	}

	return pods, nil
}

// buildNodeLocalities resolves topology labels for all unique nodes referenced by the
// given pods, served from a TTL'd in-memory cache (issue #541). On a cache miss —
// TTL lapsed or a pod referencing a node the cache has never seen (node added) —
// the whole cache is rebuilt with a single node List instead of the old serial
// per-node Gets. As before, a pod referencing a node that does not exist fails the
// listing.
func (r *KubernetesRegistry) buildNodeLocalities(ctx context.Context, pods []corev1.Pod) (map[string]locality, error) {
	// Collect unique node names
	nodeNames := make(map[string]struct{})
	for i := range pods {
		if nodeName := pods[i].Spec.NodeName; nodeName != "" {
			nodeNames[nodeName] = struct{}{}
		}
	}
	if len(nodeNames) == 0 {
		return map[string]locality{}, nil
	}

	r.nodeMu.Lock()
	defer r.nodeMu.Unlock()

	if r.nodeCache == nil || !r.now().Before(r.nodeCacheExpiry) || !nodeCacheContainsAll(r.nodeCache, nodeNames) {
		if err := r.refreshNodeCacheLocked(ctx); err != nil {
			return nil, err
		}
	}

	localities := make(map[string]locality, len(nodeNames))
	for nodeName := range nodeNames {
		loc, ok := r.nodeCache[nodeName]
		if !ok {
			// The refresh just listed every node and this one is absent: the pod
			// references a node that no longer exists. Fail the listing, matching
			// the pre-cache per-node Get behavior (NotFound propagated as error).
			r.log.ErrorContext(ctx, "failed to get node for locality", "node", nodeName)
			return nil, fmt.Errorf("failed to get node %s: node not found", nodeName)
		}
		localities[nodeName] = loc
	}

	return localities, nil
}

// refreshNodeCacheLocked rebuilds the node-locality cache from a single node List.
// Callers must hold nodeMu.
func (r *KubernetesRegistry) refreshNodeCacheLocked(ctx context.Context) error {
	var nodeList corev1.NodeList
	if err := r.reader.List(ctx, &nodeList); err != nil {
		r.log.ErrorContext(ctx, "failed to list nodes for locality", "error", err)
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	cache := make(map[string]locality, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		loc := locality{
			region: node.Labels[aetherannotations.AnnotationKubernetesNodeTopologyRegion],
			zone:   node.Labels[aetherannotations.AnnotationKubernetesNodeTopologyZone],
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				loc.nodeIP = addr.Address
				break
			}
		}
		cache[node.Name] = loc
	}

	r.nodeCache = cache
	r.nodeCacheExpiry = r.now().Add(r.nodeCacheTTL)
	return nil
}

// nodeCacheContainsAll reports whether every requested node name is present in the cache.
func nodeCacheContainsAll(cache map[string]locality, names map[string]struct{}) bool {
	for name := range names {
		if _, ok := cache[name]; !ok {
			return false
		}
	}
	return true
}

// podServesProtocol reports whether pod serves want, per its
// endpoint.aether.io/protocol annotation. UNSPECIFIED matches HTTP, which is what
// the annotation itself defaults to, so a caller that does not care still gets the
// mesh-inbound endpoints it always did.
//
// A pod whose annotation does not parse is EXCLUDED from every listing, and says so
// at WARN. The alternative — treating it as HTTP — is how a typo becomes a pod
// quietly serving the wrong protocol, which is the failure this whole change exists
// to remove. Excluding it makes the pod visibly absent instead, and the registering
// agent rejects the same value outright (endpointmeta.Protocol), so a pod in this
// state could only have been annotated after registration.
func (r *KubernetesRegistry) podServesProtocol(ctx context.Context, pod *corev1.Pod, want registryv1.Service_Protocol) bool {
	got, err := endpointmeta.Protocol(pod.Annotations)
	if err != nil {
		r.log.WarnContext(ctx, "pod has an invalid protocol annotation; excluding it from endpoint listings",
			"error", err, "pod", pod.Name, "namespace", pod.Namespace)
		return false
	}
	if want == registryv1.Service_PROTOCOL_UNSPECIFIED {
		want = registryv1.Service_PROTOCOL_HTTP
	}
	return got == want
}

// podToEndpoint converts a Kubernetes Pod to a ServiceEndpoint.
func (r *KubernetesRegistry) podToEndpoint(pod *corev1.Pod, nodeLocalities map[string]locality) (*registryv1.ServiceEndpoint, error) {
	port, err := endpointmeta.Port(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	weight, err := endpointmeta.Weight(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	// The served-port set (proposal 005 per-port EDS membership). This backend
	// never read endpoint.aether.io/ports, so Ports was nil on every endpoint it
	// returned and per-port clusters came out empty here while working on the
	// write-based backends. Found alongside #878; the same missing-parser cause.
	ports, err := endpointmeta.Ports(pod.Annotations, port)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	// Per-port L4 class (proposal 037), from the same shared parser the CNI
	// registration path uses — the two backends must not drift again (#878).
	portProtocols, err := endpointmeta.PortProtocols(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	ep := &registryv1.ServiceEndpoint{
		Ip:            pod.Status.PodIP,
		ClusterName:   r.clusterName,
		Port:          uint32(port),
		Ports:         ports,
		PortProtocols: portProtocols,
		Weight:        weight,
		Metadata:      endpointmeta.Metadata(pod.Annotations),
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			NodeName:  pod.Spec.NodeName,
			NodeIp:    nodeLocalities[pod.Spec.NodeName].nodeIP,
		},
		// This backend derives endpoints from the API server rather than receiving
		// agent registrations, so health comes from the pod's readiness condition
		// (the delegated active-HC path applies only to the write-based backends).
		Health:          podHealth(pod),
		HealthCheckMode: healthCheckModeFromAnnotations(pod.Annotations),
	}

	if loc, ok := nodeLocalities[pod.Spec.NodeName]; ok {
		ep.Locality = &registryv1.ServiceEndpoint_Locality{
			Region: loc.region,
			Zone:   loc.zone,
		}
	}

	return ep, nil
}

// podHealth maps a pod's readiness condition to the endpoint health: ready pods
// are healthy, otherwise unhealthy.
//
// A pod whose deletion has been requested is DRAINING while it is still Ready
// (aether#1124). That is the signal, at the same moment, that the node agent's
// termination watch turns into its DRAINING mark. The agent's mark reaches only
// the registrar replica it is connected to, because this backend ignores
// writes. Without this, a peer replica kept listing the pod HEALTHY through the
// whole preStop window (the kubelet keeps a terminating pod Ready until its
// containers stop), and every source agent watching that replica kept sending
// new requests into it. Once a terminating pod is no longer Ready it is
// UNHEALTHY, like the agent's own phase 2.
func podHealth(pod *corev1.Pod) registryv1.ServiceEndpoint_Health {
	ready, known := podReadyCondition(pod)
	if pod.DeletionTimestamp != nil {
		if ready {
			return registryv1.ServiceEndpoint_HEALTH_DRAINING
		}
		return registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
	}
	switch {
	case !known:
		return registryv1.ServiceEndpoint_HEALTH_UNSPECIFIED
	case ready:
		return registryv1.ServiceEndpoint_HEALTH_HEALTHY
	default:
		return registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
	}
}

// podReadyCondition returns the pod's Ready condition and whether it has one.
func podReadyCondition(pod *corev1.Pod) (ready, known bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue, true
		}
	}
	return false, false
}

func podReady(pod *corev1.Pod) bool {
	ready, _ := podReadyCondition(pod)
	return ready
}

// healthCheckModeFromAnnotations maps the endpoint.aether.io/health-check-mode
// annotation to the ServiceEndpoint health-check mode. "eds" yields EDS; "active"
// yields ACTIVE; unset yields UNSPECIFIED, which consumers treat as active (the
// default).
//
// Deliberately NOT shared with //registry/endpointmeta, although the other
// annotation parsers now are. The CNI registration path defaults an UNSET
// annotation to EDS (delegated liveness is its default); this backend must
// default to UNSPECIFIED, because it derives endpoints from the API server and
// the delegated active-HC path applies only to the write-based backends.
// Unifying the two would silently flip one of them.
func healthCheckModeFromAnnotations(annotations map[string]string) registryv1.ServiceEndpoint_HealthCheckMode {
	switch annotations[aetherannotations.AnnotationEndpointHealthCheckMode] {
	case aetherannotations.HealthCheckModeEDS:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
	case aetherannotations.HealthCheckModeActive:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_ACTIVE
	default:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_UNSPECIFIED
	}
}
