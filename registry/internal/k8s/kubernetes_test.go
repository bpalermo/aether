package k8s

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
)

// newTestRegistry constructs a KubernetesRegistry wired to a fake client built
// from the supplied Kubernetes objects.
func newTestRegistry(clusterName string, objects ...any) *KubernetesRegistry {
	b := fake.NewClientBuilder()
	for _, o := range objects {
		switch v := o.(type) {
		case *corev1.Pod:
			b = b.WithObjects(v)
		case *corev1.Node:
			b = b.WithObjects(v)
		}
	}
	reader := b.Build()
	return NewKubernetesRegistry(logr.Discard(), reader, Config{ClusterName: clusterName})
}

// managedPod returns a running pod with a PodIP and the aether managed label.
// The caller can override fields via the returned pointer before passing to newTestRegistry.
func managedPod(name, namespace, serviceAccount, podIP, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.LabelAetherManaged: "true",
			},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			NodeName:           nodeName,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: podIP,
		},
	}
}

// topologyNode returns a Node with region and zone topology labels.
func topologyNode(name, region, zone string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.AnnotationKubernetesNodeTopologyRegion: region,
				constants.AnnotationKubernetesNodeTopologyZone:   zone,
			},
		},
	}
}

// ─── Start ───────────────────────────────────────────────────────────────────

func TestStart(t *testing.T) {
	r := newTestRegistry("test-cluster")
	err := r.Start(context.Background())
	require.NoError(t, err)
}

// ─── RegisterEndpoint ────────────────────────────────────────────────────────

func TestRegisterEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		protocol registryv1.Service_Protocol
		endpoint *registryv1.ServiceEndpoint
		wantErr  bool
	}{
		{
			name:     "no-op returns nil",
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.1",
				ClusterName: "test-cluster",
				Port:        8080,
				Weight:      1024,
			},
			wantErr: false,
		},
		{
			name:     "nil endpoint returns nil",
			service:  "my-service",
			protocol: registryv1.Service_PROTOCOL_UNSPECIFIED,
			endpoint: nil,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry("test-cluster")
			err := r.RegisterEndpoint(context.Background(), tt.service, tt.protocol, tt.endpoint)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// ─── UnregisterEndpoint ──────────────────────────────────────────────────────

func TestUnregisterEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		service   string
		ipAddress string
		wantErr   bool
	}{
		{
			name:      "no-op returns nil",
			service:   "my-service",
			ipAddress: "10.0.0.1",
			wantErr:   false,
		},
		{
			name:      "empty service and address returns nil",
			service:   "",
			ipAddress: "",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry("test-cluster")
			err := r.UnregisterEndpoint(context.Background(), tt.service, tt.ipAddress)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// ─── UnregisterEndpoints ─────────────────────────────────────────────────────

func TestUnregisterEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		service     string
		ipAddresses []string
		wantErr     bool
	}{
		{
			name:        "no-op returns nil for multiple addresses",
			service:     "my-service",
			ipAddresses: []string{"10.0.0.1", "10.0.0.2"},
			wantErr:     false,
		},
		{
			name:        "no-op returns nil for empty slice",
			service:     "my-service",
			ipAddresses: []string{},
			wantErr:     false,
		},
		{
			name:        "no-op returns nil for nil slice",
			service:     "my-service",
			ipAddresses: nil,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry("test-cluster")
			err := r.UnregisterEndpoints(context.Background(), tt.service, tt.ipAddresses)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// ─── ListEndpoints ───────────────────────────────────────────────────────────

func TestListEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		objects     []any
		service     string
		protocol    registryv1.Service_Protocol
		expected    []*registryv1.ServiceEndpoint
		wantErr     bool
	}{
		{
			name:        "no pods returns empty slice",
			clusterName: "test-cluster",
			objects:     nil,
			service:     "my-service",
			protocol:    registryv1.Service_HTTP,
			expected:    nil,
			wantErr:     false,
		},
		{
			name:        "running pod with matching service account is returned",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "test-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      constants.DefaultEndpointWeight,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "us-east-1",
						Zone:   "us-east-1a",
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pod with different service account is filtered out",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "other-service", "10.0.0.1", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod not in Running phase is excluded",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Status.Phase = corev1.PodPending
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod without PodIP is excluded",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "my-service", "", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod without aether managed label is excluded",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					delete(p.Labels, constants.LabelAetherManaged)
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod with explicit port annotation uses that port",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationEndpointPort] = "9090"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "test-cluster",
					Port:        9090,
					Weight:      constants.DefaultEndpointWeight,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "us-east-1",
						Zone:   "us-east-1a",
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pod with explicit weight annotation uses that weight",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationEndpointWeight] = "512"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "test-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      512,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "us-east-1",
						Zone:   "us-east-1a",
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pod with metadata annotations includes metadata in endpoint",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationAetherEndpointMetadataPrefix+"version"] = "v2"
					p.Annotations[constants.AnnotationAetherEndpointMetadataPrefix+"env"] = "production"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "test-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      constants.DefaultEndpointWeight,
					Metadata: map[string]string{
						"version": "v2",
						"env":     "production",
					},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "us-east-1",
						Zone:   "us-east-1a",
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pod with invalid port annotation is skipped without error",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationEndpointPort] = "not-a-port"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			// The pod is skipped (error logged, not propagated), so no endpoints are returned.
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod with invalid weight annotation is skipped without error",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationEndpointWeight] = "not-a-weight"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "missing node returns error",
			clusterName: "test-cluster",
			objects: []any{
				// Pod references node-1 but the node is not in the fake client.
				managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  true,
		},
		{
			name:        "pod on node with no topology labels has nil locality",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1"),
				// Node with no topology labels.
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-1",
						Labels: map[string]string{},
					},
				},
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "test-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      constants.DefaultEndpointWeight,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "",
						Zone:   "",
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "multiple pods with matching service account are all returned",
			clusterName: "prod-cluster",
			objects: []any{
				managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1"),
				managedPod("pod-b", "default", "my-service", "10.0.0.2", "node-1"),
				managedPod("pod-c", "default", "other-service", "10.0.0.3", "node-1"),
				topologyNode("node-1", "eu-west-1", "eu-west-1b"),
			},
			service:  "my-service",
			protocol: registryv1.Service_HTTP,
			expected: []*registryv1.ServiceEndpoint{
				{
					Ip:          "10.0.0.1",
					ClusterName: "prod-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      constants.DefaultEndpointWeight,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-a",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "eu-west-1",
						Zone:   "eu-west-1b",
					},
				},
				{
					Ip:          "10.0.0.2",
					ClusterName: "prod-cluster",
					Port:        uint32(constants.DefaultEndpointPort),
					Weight:      constants.DefaultEndpointWeight,
					Metadata:    map[string]string{},
					KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
						Namespace: "default",
						PodName:   "pod-b",
						NodeName:  "node-1",
					},
					Locality: &registryv1.ServiceEndpoint_Locality{
						Region: "eu-west-1",
						Zone:   "eu-west-1b",
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry(tt.clusterName, tt.objects...)
			got, err := r.ListEndpoints(context.Background(), tt.service, tt.protocol)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ─── ListAllEndpoints ────────────────────────────────────────────────────────

func TestListAllEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		objects     []any
		protocol    registryv1.Service_Protocol
		expected    map[string][]*registryv1.ServiceEndpoint
		wantErr     bool
	}{
		{
			name:        "no pods returns empty map",
			clusterName: "test-cluster",
			objects:     nil,
			protocol:    registryv1.Service_HTTP,
			expected:    map[string][]*registryv1.ServiceEndpoint{},
			wantErr:     false,
		},
		{
			name:        "single pod returns map with one service",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {
					{
						Ip:          "10.0.0.1",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-a",
							NodeName:  "node-1",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-east-1",
							Zone:   "us-east-1a",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pods with multiple service accounts are grouped correctly",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1"),
				managedPod("pod-b", "default", "svc-b", "10.0.0.2", "node-1"),
				managedPod("pod-c", "default", "svc-a", "10.0.0.3", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {
					{
						Ip:          "10.0.0.1",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-a",
							NodeName:  "node-1",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-east-1",
							Zone:   "us-east-1a",
						},
					},
					{
						Ip:          "10.0.0.3",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-c",
							NodeName:  "node-1",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-east-1",
							Zone:   "us-east-1a",
						},
					},
				},
				"svc-b": {
					{
						Ip:          "10.0.0.2",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-b",
							NodeName:  "node-1",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-east-1",
							Zone:   "us-east-1a",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "pod without service account is skipped",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "", "10.0.0.1", "node-1")
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "pod not in Running phase is excluded",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1")
					p.Status.Phase = corev1.PodFailed
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "pod without PodIP is excluded",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "", "node-1"),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "pod with invalid port annotation is skipped",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1")
					p.Annotations[constants.AnnotationEndpointPort] = "99999999"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "missing node returns error",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-missing"),
			},
			protocol: registryv1.Service_HTTP,
			expected: nil,
			wantErr:  true,
		},
		{
			name:        "pods on different nodes resolve correct localities",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-west"),
				managedPod("pod-b", "default", "svc-a", "10.0.0.2", "node-east"),
				topologyNode("node-west", "us-west-2", "us-west-2a"),
				topologyNode("node-east", "us-east-1", "us-east-1b"),
			},
			protocol: registryv1.Service_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {
					{
						Ip:          "10.0.0.1",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-a",
							NodeName:  "node-west",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-west-2",
							Zone:   "us-west-2a",
						},
					},
					{
						Ip:          "10.0.0.2",
						ClusterName: "test-cluster",
						Port:        uint32(constants.DefaultEndpointPort),
						Weight:      constants.DefaultEndpointWeight,
						Metadata:    map[string]string{},
						KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
							Namespace: "default",
							PodName:   "pod-b",
							NodeName:  "node-east",
						},
						Locality: &registryv1.ServiceEndpoint_Locality{
							Region: "us-east-1",
							Zone:   "us-east-1b",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRegistry(tt.clusterName, tt.objects...)
			got, err := r.ListAllEndpoints(context.Background(), tt.protocol)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ─── Annotation parsing helpers (unit-level) ─────────────────────────────────

func TestGetPortFromAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    uint16
		wantErr     bool
	}{
		{
			name:        "no annotation returns default port",
			annotations: map[string]string{},
			expected:    constants.DefaultEndpointPort,
			wantErr:     false,
		},
		{
			name:        "nil annotations returns default port",
			annotations: nil,
			expected:    constants.DefaultEndpointPort,
			wantErr:     false,
		},
		{
			name:        "valid port annotation is parsed",
			annotations: map[string]string{constants.AnnotationEndpointPort: "9090"},
			expected:    9090,
			wantErr:     false,
		},
		{
			name:        "minimum port value 1 is valid",
			annotations: map[string]string{constants.AnnotationEndpointPort: "1"},
			expected:    1,
			wantErr:     false,
		},
		{
			name:        "maximum port value 65535 is valid",
			annotations: map[string]string{constants.AnnotationEndpointPort: "65535"},
			expected:    65535,
			wantErr:     false,
		},
		{
			name:        "port value exceeding uint16 max returns error",
			annotations: map[string]string{constants.AnnotationEndpointPort: "65536"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "non-numeric port annotation returns error",
			annotations: map[string]string{constants.AnnotationEndpointPort: "not-a-port"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "negative port annotation returns error",
			annotations: map[string]string{constants.AnnotationEndpointPort: "-1"},
			expected:    0,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getPortFromAnnotations(tt.annotations)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestGetWeightFromAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    uint32
		wantErr     bool
	}{
		{
			name:        "no annotation returns default weight",
			annotations: map[string]string{},
			expected:    constants.DefaultEndpointWeight,
			wantErr:     false,
		},
		{
			name:        "nil annotations returns default weight",
			annotations: nil,
			expected:    constants.DefaultEndpointWeight,
			wantErr:     false,
		},
		{
			name:        "valid weight annotation is parsed",
			annotations: map[string]string{constants.AnnotationEndpointWeight: "512"},
			expected:    512,
			wantErr:     false,
		},
		{
			name:        "weight of zero is valid",
			annotations: map[string]string{constants.AnnotationEndpointWeight: "0"},
			expected:    0,
			wantErr:     false,
		},
		{
			name:        "maximum uint32 weight is valid",
			annotations: map[string]string{constants.AnnotationEndpointWeight: "4294967295"},
			expected:    4294967295,
			wantErr:     false,
		},
		{
			name:        "weight exceeding uint32 max returns error",
			annotations: map[string]string{constants.AnnotationEndpointWeight: "4294967296"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "non-numeric weight annotation returns error",
			annotations: map[string]string{constants.AnnotationEndpointWeight: "heavy"},
			expected:    0,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getWeightFromAnnotations(tt.annotations)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestGetEndpointMetadataFromAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    map[string]string
	}{
		{
			name:        "nil annotations returns empty map",
			annotations: nil,
			expected:    map[string]string{},
		},
		{
			name:        "empty annotations returns empty map",
			annotations: map[string]string{},
			expected:    map[string]string{},
		},
		{
			name: "non-metadata annotations are ignored",
			annotations: map[string]string{
				"some.other.annotation/key": "value",
				constants.AnnotationEndpointPort: "8080",
			},
			expected: map[string]string{},
		},
		{
			name: "single metadata annotation is extracted with key suffix",
			annotations: map[string]string{
				constants.AnnotationAetherEndpointMetadataPrefix + "version": "v2",
			},
			expected: map[string]string{
				"version": "v2",
			},
		},
		{
			name: "multiple metadata annotations are all extracted",
			annotations: map[string]string{
				constants.AnnotationAetherEndpointMetadataPrefix + "version": "v2",
				constants.AnnotationAetherEndpointMetadataPrefix + "env":     "production",
				constants.AnnotationAetherEndpointMetadataPrefix + "tier":    "backend",
			},
			expected: map[string]string{
				"version": "v2",
				"env":     "production",
				"tier":    "backend",
			},
		},
		{
			name: "annotation key equal to prefix alone is not extracted",
			annotations: map[string]string{
				constants.AnnotationAetherEndpointMetadataPrefix: "value",
			},
			expected: map[string]string{},
		},
		{
			name: "metadata annotations mixed with non-metadata annotations extracts only metadata",
			annotations: map[string]string{
				constants.AnnotationAetherEndpointMetadataPrefix + "canary": "true",
				constants.AnnotationEndpointPort:   "9090",
				constants.AnnotationEndpointWeight: "256",
			},
			expected: map[string]string{
				"canary": "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getEndpointMetadataFromAnnotations(tt.annotations)
			assert.Equal(t, tt.expected, got)
		})
	}
}
