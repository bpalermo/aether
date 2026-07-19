// Package k8s implements the Registry interface using the Kubernetes API server as the backend.
// It discovers service endpoints by listing pods with the aether.io/managed=true label
// and derives service names from each pod's ServiceAccount. Node topology labels provide
// region and zone locality information.
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/serviceref"
)

// Config holds the configuration for the Kubernetes registry backend.
type Config struct {
	// ClusterName is the name of the cluster, used to populate ServiceEndpoint.ClusterName.
	ClusterName string
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
type KubernetesRegistry struct {
	log         *slog.Logger
	clusterName string
	reader      client.Reader

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
	return &KubernetesRegistry{
		log:          commonlog.Named(log, "registry-kubernetes"),
		clusterName:  cfg.ClusterName,
		reader:       reader,
		nodeCacheTTL: nodeLocalityCacheTTL,
		now:          time.Now,
	}
}

// Initialize is a no-op for the Kubernetes registry.
// The API server connection is managed by the controller-runtime manager.
func (r *KubernetesRegistry) Initialize(_ context.Context) error {
	r.log.Info("kubernetes registry initialized", "cluster", r.clusterName)
	return nil
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
// The Kubernetes registry derives endpoints from managed pods, every one of which is a
// mesh-inbound (HTTP/h2 over :15008) endpoint by construction — it has no notion of a
// per-pod TCP-only service. A TCP query must therefore return NOTHING, not the same
// HTTP endpoint set: the agent's LoadClustersFromRegistry builds a service's HTTP
// cluster (with its outbound/cap_http vhost) from the HTTP listing, then in a second
// pass OVERWRITES the same map key with a vhost-less tcp:true entry from the TCP
// listing ("a service is HTTP or TCP, never both" — true for etcd/ddb, which key by
// protocol). Returning the pods for TCP too violated that invariant: every mesh
// service collapsed to a TCP-only entry, its CDS cluster + GAMMA cap_http vhost
// vanished, and captured requests routing to it 503'd (no_healthy_upstream). Treat
// HTTP/UNSPECIFIED as the served protocol; TCP yields no endpoints.
func (r *KubernetesRegistry) ListEndpoints(ctx context.Context, service string, protocol registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing endpoints", "service", service)

	if protocol == registryv1.Service_PROTOCOL_TCP {
		return nil, nil
	}

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
// Every managed-pod endpoint is a mesh-inbound HTTP/h2 endpoint (see ListEndpoints).
// A TCP query returns an empty map so the agent's TCP cluster pass does not clobber
// the HTTP cluster/vhost entries built from the HTTP listing (HTTP/UNSPECIFIED is the
// served protocol).
func (r *KubernetesRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.DebugContext(ctx, "listing all endpoints")

	if protocol == registryv1.Service_PROTOCOL_TCP {
		return map[string][]*registryv1.ServiceEndpoint{}, nil
	}

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
		client.MatchingLabels{constants.LabelAetherManaged: "true"},
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
			region: node.Labels[constants.AnnotationKubernetesNodeTopologyRegion],
			zone:   node.Labels[constants.AnnotationKubernetesNodeTopologyZone],
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

// podToEndpoint converts a Kubernetes Pod to a ServiceEndpoint.
func (r *KubernetesRegistry) podToEndpoint(pod *corev1.Pod, nodeLocalities map[string]locality) (*registryv1.ServiceEndpoint, error) {
	port, err := getPortFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	weight, err := getWeightFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	ep := &registryv1.ServiceEndpoint{
		Ip:          pod.Status.PodIP,
		ClusterName: r.clusterName,
		Port:        uint32(port),
		Weight:      weight,
		Metadata:    getEndpointMetadataFromAnnotations(pod.Annotations),
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
func podHealth(pod *corev1.Pod) registryv1.ServiceEndpoint_Health {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			if c.Status == corev1.ConditionTrue {
				return registryv1.ServiceEndpoint_HEALTH_HEALTHY
			}
			return registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		}
	}
	return registryv1.ServiceEndpoint_HEALTH_UNSPECIFIED
}

// healthCheckModeFromAnnotations maps the endpoint.aether.io/health-check-mode
// annotation to the ServiceEndpoint health-check mode. "eds" yields EDS; "active"
// yields ACTIVE; unset yields UNSPECIFIED, which consumers treat as active (the
// default).
func healthCheckModeFromAnnotations(annotations map[string]string) registryv1.ServiceEndpoint_HealthCheckMode {
	switch annotations[constants.AnnotationEndpointHealthCheckMode] {
	case constants.HealthCheckModeEDS:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
	case constants.HealthCheckModeActive:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_ACTIVE
	default:
		return registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_UNSPECIFIED
	}
}

// getPortFromAnnotations extracts the endpoint port from pod annotations.
func getPortFromAnnotations(annotations map[string]string) (uint16, error) {
	s, ok := annotations[constants.AnnotationEndpointPort]
	if !ok {
		return constants.DefaultEndpointPort, nil
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port annotation: %w", err)
	}
	return uint16(port), nil
}

// getWeightFromAnnotations extracts the endpoint weight from pod annotations.
func getWeightFromAnnotations(annotations map[string]string) (uint32, error) {
	s, ok := annotations[constants.AnnotationEndpointWeight]
	if !ok {
		return constants.DefaultEndpointWeight, nil
	}
	weight, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid weight annotation: %w", err)
	}
	return uint32(weight), nil
}

// getEndpointMetadataFromAnnotations extracts endpoint metadata from pod annotations.
func getEndpointMetadataFromAnnotations(annotations map[string]string) map[string]string {
	metadata := map[string]string{}
	prefix := constants.AnnotationAetherEndpointMetadataPrefix
	for key, value := range annotations {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			metadata[key[len(prefix):]] = value
		}
	}
	return metadata
}
