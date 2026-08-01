package k8s

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	aetherannotations "github.com/bpalermo/aether/common/constants/annotations"
	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	"github.com/bpalermo/aether/registry/registrytest"
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
	return NewKubernetesRegistry(slog.New(slog.DiscardHandler), reader, Config{ClusterName: clusterName})
}

// managedPod returns a running pod with a PodIP and the aether managed label.
// The caller can override fields via the returned pointer before passing to newTestRegistry.
func managedPod(name, namespace, serviceAccount, podIP, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				aetherlabels.LabelAetherManaged: "true",
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
				aetherannotations.AnnotationKubernetesNodeTopologyRegion: region,
				aetherannotations.AnnotationKubernetesNodeTopologyZone:   zone,
			},
		},
	}
}

// ─── Initialize ──────────────────────────────────────────────────────────────

func TestInitialize(t *testing.T) {
	r := newTestRegistry("test-cluster")
	err := r.Initialize(context.Background())
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
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
			service:     "default/my-service",
			ipAddresses: []string{"10.0.0.1", "10.0.0.2"},
			wantErr:     false,
		},
		{
			name:        "no-op returns nil for empty slice",
			service:     "default/my-service",
			ipAddresses: []string{},
			wantErr:     false,
		},
		{
			name:        "no-op returns nil for nil slice",
			service:     "default/my-service",
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
			service:     "default/my-service",
			protocol:    registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			name:        "pod readiness is derived (parity with other backends)",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
					Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod without aether managed label is excluded",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					delete(p.Labels, aetherlabels.LabelAetherManaged)
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: nil,
			wantErr:  false,
		},
		{
			name:        "pod with explicit port annotation uses that port",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[aetherannotations.AnnotationEndpointPort] = "9090"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
					p.Annotations[aetherannotations.AnnotationEndpointWeight] = "512"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
					p.Annotations[aetherannotations.AnnotationAetherEndpointMetadataPrefix+"version"] = "v2"
					p.Annotations[aetherannotations.AnnotationAetherEndpointMetadataPrefix+"env"] = "production"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			name:        "pod with eds health-check-mode annotation sets EDS mode",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "my-service", "10.0.0.1", "node-1")
					p.Annotations[aetherannotations.AnnotationEndpointHealthCheckMode] = aetherannotations.HealthCheckModeEDS
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
					HealthCheckMode: registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS,
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
					p.Annotations[aetherannotations.AnnotationEndpointPort] = "not-a-port"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
					p.Annotations[aetherannotations.AnnotationEndpointWeight] = "not-a-weight"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			service:  "default/my-service",
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			protocol:    registryv1.Service_PROTOCOL_HTTP,
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"default/svc-a": {
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"default/svc-a": {
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
				"default/svc-b": {
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "pod with invalid port annotation is skipped",
			clusterName: "test-cluster",
			objects: []any{
				func() *corev1.Pod {
					p := managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1")
					p.Annotations[aetherannotations.AnnotationEndpointPort] = "99999999"
					return p
				}(),
				topologyNode("node-1", "us-east-1", "us-east-1a"),
			},
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{},
			wantErr:  false,
		},
		{
			name:        "missing node returns error",
			clusterName: "test-cluster",
			objects: []any{
				managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-missing"),
			},
			protocol: registryv1.Service_PROTOCOL_HTTP,
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
			protocol: registryv1.Service_PROTOCOL_HTTP,
			expected: map[string][]*registryv1.ServiceEndpoint{
				"default/svc-a": {
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
			// Shared cross-backend contract: keys must be namespace-qualified.
			registrytest.RequireNamespaceQualifiedKeys(t, got)
		})
	}
}

// TestListAllEndpoints_SameSANamespaceIsolation is the regression guard for the
// 020 keying bug: two pods sharing a ServiceAccount name across DIFFERENT
// namespaces must produce TWO distinct "<ns>/<sa>" keys, not collide under the
// bare SA. The collision merged their endpoint sets into one entry whose ordering
// then oscillated and churned the agent's xDS snapshot (the MESH-HTTP MeshFrontend
// flap). Matches the namespace-qualified keying of the CNI/etcd/ddb paths.
func TestListAllEndpoints_SameSANamespaceIsolation(t *testing.T) {
	r := newTestRegistry(
		"c",
		managedPod("echo-a", "team-a", "echo-v1", "10.0.0.1", "node-1"),
		managedPod("echo-b", "team-b", "echo-v1", "10.0.0.2", "node-1"),
		topologyNode("node-1", "us-east-1", "us-east-1a"),
	)

	got, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, got, 2, "same SA in two namespaces must not collide into one key")
	require.Len(t, got["team-a/echo-v1"], 1)
	assert.Equal(t, "10.0.0.1", got["team-a/echo-v1"][0].GetIp())
	require.Len(t, got["team-b/echo-v1"], 1)
	assert.Equal(t, "10.0.0.2", got["team-b/echo-v1"][0].GetIp())

	// The single-service read isolates by namespace too (matches by "<ns>/<sa>").
	epsA, err := r.ListEndpoints(context.Background(), "team-a/echo-v1", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, epsA, 1)
	assert.Equal(t, "10.0.0.1", epsA[0].GetIp())
}

// TestKubernetesRegistry_TCPProtocolReturnsEmpty is the regression guard for the
// MESH-HTTP RequestHeaderModifier/MeshFrontend 503 bug: the Kubernetes registry
// derives every endpoint from a managed pod's mesh-inbound (HTTP/h2) listener and
// has no per-pod TCP service. Returning the same pods for a PROTOCOL_TCP query made
// the agent's LoadClustersFromRegistry overwrite each service's HTTP cluster+vhost
// with a vhost-less tcp:true entry, so the CDS cluster and GAMMA cap_http vhost
// disappeared and captured requests 503'd. A TCP query must yield no endpoints;
// HTTP and UNSPECIFIED return the managed-pod endpoints.
func TestKubernetesRegistry_TCPProtocolReturnsEmpty(t *testing.T) {
	r := newTestRegistry(
		"c",
		managedPod("echo-a", "team-a", "echo-v1", "10.0.0.1", "node-1"),
		topologyNode("node-1", "us-east-1", "us-east-1a"),
	)

	// HTTP sees the managed pod.
	httpAll, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, httpAll, 1)

	// UNSPECIFIED is treated as the served (HTTP) protocol too.
	unspecAll, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_UNSPECIFIED)
	require.NoError(t, err)
	require.Len(t, unspecAll, 1)

	// TCP returns an empty (non-nil) map so the agent's TCP cluster pass does not
	// clobber the HTTP cluster/vhost entries.
	tcpAll, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_TCP)
	require.NoError(t, err)
	require.NotNil(t, tcpAll)
	require.Empty(t, tcpAll)

	// Shared cross-backend contract: a service serves HTTP xor TCP, never both.
	registrytest.RequireProtocolDisjoint(t, httpAll, tcpAll)

	// Single-service TCP read is empty as well.
	tcpOne, err := r.ListEndpoints(context.Background(), "team-a/echo-v1", registryv1.Service_PROTOCOL_TCP)
	require.NoError(t, err)
	require.Empty(t, tcpOne)

	httpOne, err := r.ListEndpoints(context.Background(), "team-a/echo-v1", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, httpOne, 1)
}

// ─── Node-locality cache (issue #541) ────────────────────────────────────────

// nodeReadCounter tracks how many node reads (List/Get) hit the underlying client.
type nodeReadCounter struct {
	lists int
	gets  int
}

// newCountingRegistry builds a KubernetesRegistry over a fake client whose node
// reads are counted, returning the registry, the writable client (to mutate the
// cluster mid-test), and the counter.
func newCountingRegistry(objects ...client.Object) (*KubernetesRegistry, client.WithWatch, *nodeReadCounter) {
	counter := &nodeReadCounter{}
	c := fake.NewClientBuilder().
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					counter.lists++
				}
				return cl.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					counter.gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	return NewKubernetesRegistry(slog.New(slog.DiscardHandler), c, Config{ClusterName: "test-cluster"}), c, counter
}

// TestNodeLocalityCache_HitAvoidsNodeReads verifies the perf contract of #541:
// repeated listings within the TTL resolve node localities from the cache — one
// node List on the first call, zero node reads afterwards, and never per-node Gets.
func TestNodeLocalityCache_HitAvoidsNodeReads(t *testing.T) {
	r, _, counter := newCountingRegistry(
		managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1"),
		managedPod("pod-b", "default", "svc-b", "10.0.0.2", "node-2"),
		topologyNode("node-1", "us-east-1", "us-east-1a"),
		topologyNode("node-2", "us-east-1", "us-east-1b"),
	)

	first, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, 1, counter.lists, "first listing refreshes the cache with a single node List")
	assert.Equal(t, 0, counter.gets, "node localities must never be resolved via per-node Gets")

	// Repeated reloads (the hot path) are served entirely from the cache.
	for range 5 {
		got, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
		require.NoError(t, err)
		assert.Equal(t, first, got, "cached localities must yield identical endpoints")
	}
	eps, err := r.ListEndpoints(context.Background(), "default/svc-a", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "us-east-1a", eps[0].GetLocality().GetZone())

	assert.Equal(t, 1, counter.lists, "cache hits must not re-list nodes")
	assert.Equal(t, 0, counter.gets)
}

// TestNodeLocalityCache_NodeAddedForcesRefresh verifies that a pod scheduled onto
// a node the cache has never seen refreshes the cache immediately (no TTL wait)
// and resolves the new node's locality correctly.
func TestNodeLocalityCache_NodeAddedForcesRefresh(t *testing.T) {
	r, c, counter := newCountingRegistry(
		managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1"),
		topologyNode("node-1", "us-east-1", "us-east-1a"),
	)
	ctx := context.Background()

	_, err := r.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Equal(t, 1, counter.lists)

	// A new node joins and a managed pod lands on it.
	require.NoError(t, c.Create(ctx, topologyNode("node-2", "eu-west-1", "eu-west-1b")))
	require.NoError(t, c.Create(ctx, managedPod("pod-b", "default", "svc-a", "10.0.0.2", "node-2")))

	got, err := r.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, got["default/svc-a"], 2)
	byPod := map[string]*registryv1.ServiceEndpoint{}
	for _, ep := range got["default/svc-a"] {
		byPod[ep.GetKubernetesMetadata().GetPodName()] = ep
	}
	require.Contains(t, byPod, "pod-b")
	assert.Equal(t, "eu-west-1", byPod["pod-b"].GetLocality().GetRegion())
	assert.Equal(t, "eu-west-1b", byPod["pod-b"].GetLocality().GetZone())
	assert.Equal(t, 2, counter.lists, "unknown node must force a cache refresh")

	// The refreshed cache serves subsequent calls without new node reads.
	_, err = r.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	assert.Equal(t, 2, counter.lists)
	assert.Equal(t, 0, counter.gets)
}

// TestNodeLocalityCache_TTLExpiryRefreshes verifies the staleness bound: a node
// label change is served stale within the TTL and picked up after it lapses.
func TestNodeLocalityCache_TTLExpiryRefreshes(t *testing.T) {
	r, c, counter := newCountingRegistry(
		managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-1"),
		topologyNode("node-1", "us-east-1", "us-east-1a"),
	)
	ctx := context.Background()

	// Deterministic clock.
	now := time.Now()
	r.now = func() time.Time { return now }

	first, err := r.ListEndpoints(ctx, "default/svc-a", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "us-east-1a", first[0].GetLocality().GetZone())
	require.Equal(t, 1, counter.lists)

	// The node's zone label changes (essentially never happens in practice).
	var node corev1.Node
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "node-1"}, &node))
	node.Labels[aetherannotations.AnnotationKubernetesNodeTopologyZone] = "us-east-1z"
	require.NoError(t, c.Update(ctx, &node))

	// Within the TTL the cached locality is served.
	stale, err := r.ListEndpoints(ctx, "default/svc-a", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	assert.Equal(t, "us-east-1a", stale[0].GetLocality().GetZone())
	assert.Equal(t, 1, counter.lists)

	// After the TTL lapses the next listing refreshes and observes the change.
	now = now.Add(nodeLocalityCacheTTL + time.Second)
	fresh, err := r.ListEndpoints(ctx, "default/svc-a", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	assert.Equal(t, "us-east-1z", fresh[0].GetLocality().GetZone())
	assert.Equal(t, 2, counter.lists)
}

// TestNodeLocalityCache_MissingNodeStillErrors pins the pre-cache error contract:
// a managed pod referencing a node that does not exist fails the listing even
// though node resolution now goes through the cache, and the failure is not
// poisoned into the cache (the node appearing later heals the listing).
func TestNodeLocalityCache_MissingNodeStillErrors(t *testing.T) {
	r, c, _ := newCountingRegistry(
		managedPod("pod-a", "default", "svc-a", "10.0.0.1", "node-gone"),
	)
	ctx := context.Background()

	_, err := r.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	require.ErrorContains(t, err, "node-gone")

	_, err = r.ListEndpoints(ctx, "default/svc-a", registryv1.Service_PROTOCOL_HTTP)
	require.ErrorContains(t, err, "node-gone")

	// The node shows up (e.g., registration race): the next listing succeeds.
	require.NoError(t, c.Create(ctx, topologyNode("node-gone", "us-east-1", "us-east-1a")))
	got, err := r.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, got["default/svc-a"], 1)
	assert.Equal(t, "us-east-1a", got["default/svc-a"][0].GetLocality().GetZone())
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
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "9090"},
			expected:    9090,
			wantErr:     false,
		},
		{
			name:        "minimum port value 1 is valid",
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "1"},
			expected:    1,
			wantErr:     false,
		},
		{
			name:        "maximum port value 65535 is valid",
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "65535"},
			expected:    65535,
			wantErr:     false,
		},
		{
			name:        "port value exceeding uint16 max returns error",
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "65536"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "non-numeric port annotation returns error",
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "not-a-port"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "negative port annotation returns error",
			annotations: map[string]string{aetherannotations.AnnotationEndpointPort: "-1"},
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
			annotations: map[string]string{aetherannotations.AnnotationEndpointWeight: "512"},
			expected:    512,
			wantErr:     false,
		},
		{
			name:        "weight of zero is valid",
			annotations: map[string]string{aetherannotations.AnnotationEndpointWeight: "0"},
			expected:    0,
			wantErr:     false,
		},
		{
			name:        "maximum uint32 weight is valid",
			annotations: map[string]string{aetherannotations.AnnotationEndpointWeight: "4294967295"},
			expected:    4294967295,
			wantErr:     false,
		},
		{
			name:        "weight exceeding uint32 max returns error",
			annotations: map[string]string{aetherannotations.AnnotationEndpointWeight: "4294967296"},
			expected:    0,
			wantErr:     true,
		},
		{
			name:        "non-numeric weight annotation returns error",
			annotations: map[string]string{aetherannotations.AnnotationEndpointWeight: "heavy"},
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
				"some.other.annotation/key":              "value",
				aetherannotations.AnnotationEndpointPort: "8080",
			},
			expected: map[string]string{},
		},
		{
			name: "single metadata annotation is extracted with key suffix",
			annotations: map[string]string{
				aetherannotations.AnnotationAetherEndpointMetadataPrefix + "version": "v2",
			},
			expected: map[string]string{
				"version": "v2",
			},
		},
		{
			name: "multiple metadata annotations are all extracted",
			annotations: map[string]string{
				aetherannotations.AnnotationAetherEndpointMetadataPrefix + "version": "v2",
				aetherannotations.AnnotationAetherEndpointMetadataPrefix + "env":     "production",
				aetherannotations.AnnotationAetherEndpointMetadataPrefix + "tier":    "backend",
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
				aetherannotations.AnnotationAetherEndpointMetadataPrefix: "value",
			},
			expected: map[string]string{},
		},
		{
			name: "metadata annotations mixed with non-metadata annotations extracts only metadata",
			annotations: map[string]string{
				aetherannotations.AnnotationAetherEndpointMetadataPrefix + "canary": "true",
				aetherannotations.AnnotationEndpointPort:                            "9090",
				aetherannotations.AnnotationEndpointWeight:                          "256",
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
