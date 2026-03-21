// Package k8s implements the Registry interface using the Kubernetes API server as the backend.
// It discovers service endpoints by listing pods with the aether.io/managed=true label
// and derives service names from each pod's ServiceAccount. Node topology labels provide
// region and zone locality information.
package k8s

import (
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
)

// Config holds the configuration for the Kubernetes registry backend.
type Config struct {
	// ClusterName is the name of the cluster, used to populate ServiceEndpoint.ClusterName.
	ClusterName string
}

// KubernetesRegistry is a Registry implementation backed by the Kubernetes API server.
// It reads pods labeled with aether.io/managed=true and converts them to ServiceEndpoints.
// Write operations (Register/Unregister) are no-ops since the API server is the source of truth.
type KubernetesRegistry struct {
	log         logr.Logger
	clusterName string
	reader      client.Reader
}

// NewKubernetesRegistry creates a new Kubernetes API server backed Registry.
// The reader should be a direct API reader (e.g., manager.GetAPIReader()) to avoid
// cache synchronization issues during startup.
func NewKubernetesRegistry(log logr.Logger, reader client.Reader, cfg Config) *KubernetesRegistry {
	return &KubernetesRegistry{
		log:         log.WithName("registry-kubernetes"),
		clusterName: cfg.ClusterName,
		reader:      reader,
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
func (r *KubernetesRegistry) ListEndpoints(ctx context.Context, service string, _ registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing endpoints", "service", service)

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
		if pod.Spec.ServiceAccountName != service {
			continue
		}
		ep, err := r.podToEndpoint(pod, nodeLocalities)
		if err != nil {
			r.log.Error(err, "failed to convert pod to endpoint", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		endpoints = append(endpoints, ep)
	}

	r.log.V(1).Info("listed endpoints", "service", service, "count", len(endpoints))
	return endpoints, nil
}

// ListAllEndpoints returns all endpoints for all services, grouped by service name (ServiceAccount).
// It lists all managed pods, resolves node localities, and converts each pod to a ServiceEndpoint.
func (r *KubernetesRegistry) ListAllEndpoints(ctx context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.log.V(1).Info("listing all endpoints")

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
		serviceName := pod.Spec.ServiceAccountName
		if serviceName == "" {
			r.log.V(1).Info("skipping pod without service account", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}

		ep, err := r.podToEndpoint(pod, nodeLocalities)
		if err != nil {
			r.log.Error(err, "failed to convert pod to endpoint", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		endpointsByService[serviceName] = append(endpointsByService[serviceName], ep)
	}

	r.log.V(1).Info("listed all endpoints", "services", len(endpointsByService))
	return endpointsByService, nil
}

// locality holds the region and zone for a node.
type locality struct {
	region string
	zone   string
}

// listManagedPods lists all running pods with the aether.io/managed=true label that have a PodIP.
func (r *KubernetesRegistry) listManagedPods(ctx context.Context) ([]corev1.Pod, error) {
	var podList corev1.PodList
	err := r.reader.List(ctx, &podList,
		client.MatchingLabels{constants.LabelAetherManaged: "true"},
	)
	if err != nil {
		r.log.Error(err, "failed to list managed pods")
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

// buildNodeLocalities fetches topology labels for all unique nodes referenced by the given pods.
// Results are cached by node name to avoid redundant API calls.
func (r *KubernetesRegistry) buildNodeLocalities(ctx context.Context, pods []corev1.Pod) (map[string]locality, error) {
	// Collect unique node names
	nodeNames := make(map[string]struct{})
	for i := range pods {
		if nodeName := pods[i].Spec.NodeName; nodeName != "" {
			nodeNames[nodeName] = struct{}{}
		}
	}

	localities := make(map[string]locality, len(nodeNames))
	for nodeName := range nodeNames {
		var node corev1.Node
		if err := r.reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			r.log.Error(err, "failed to get node for locality", "node", nodeName)
			return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}
		localities[nodeName] = locality{
			region: node.Labels[constants.AnnotationKubernetesNodeTopologyRegion],
			zone:   node.Labels[constants.AnnotationKubernetesNodeTopologyZone],
		}
	}

	return localities, nil
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
		},
	}

	if loc, ok := nodeLocalities[pod.Spec.NodeName]; ok {
		ep.Locality = &registryv1.ServiceEndpoint_Locality{
			Region: loc.region,
			Zone:   loc.zone,
		}
	}

	return ep, nil
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
