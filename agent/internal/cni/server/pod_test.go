package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/pkg/storage"
	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	agentconstants "github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/constants"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testRegistry is a configurable mock implementing registry.Registry for use in server tests.
type testRegistry struct {
	registerEndpointErr    error
	unregisterEndpointsErr error
}

var _ registry.Registry = (*testRegistry)(nil)

func (r *testRegistry) Initialize(_ context.Context) error { return nil }
func (r *testRegistry) Close() error                        { return nil }

func (r *testRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	return r.registerEndpointErr
}

func (r *testRegistry) UnregisterEndpoint(_ context.Context, _ string, _ string) error {
	return nil
}

func (r *testRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	return r.unregisterEndpointsErr
}

func (r *testRegistry) ListEndpoints(_ context.Context, _ string, _ registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (r *testRegistry) ListAllEndpoints(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	return map[string][]*registryv1.ServiceEndpoint{}, nil
}

// newTestCNIServer constructs a bare CNIServer without the gRPC/socket machinery, suitable
// for unit-testing the AddPod and RemovePod methods directly.
func newTestCNIServer(k8sClient client.Client, stor storage.Storage[*cniv1.CNIPod], reg registry.Registry, sc *cache.SnapshotCache) *CNIServer {
	return &CNIServer{
		log:           logr.Discard(),
		clusterName:   "test-cluster",
		nodeName:      "test-node",
		trustDomain:   "example.org",
		nodeRegion:    "us-east-1",
		nodeZone:      "us-east-1a",
		storage:       stor,
		registry:      reg,
		snapshotCache: sc,
		spireBridge:   spire.NewBridge(agentconstants.DefaultSpireAdminSocketPath, sc, logr.Discard()),
		k8sClient:     k8sClient,
	}
}

// validCNIPod returns a CNIPod that is not ignorable and carries the labels and annotations
// required by NewServiceEndpointFromCNIPod.
func validCNIPod(name, namespace, containerID string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             name,
		Namespace:        namespace,
		NetworkNamespace: "/proc/1234/ns/net",
		ContainerId:      containerID,
		Ips:              []string{"10.0.0.1"},
		ServiceAccount:   "default",
		Labels: map[string]string{
			constants.LabelAetherManaged: "true",
		},
		Annotations: map[string]string{},
	}
}

// validK8sPod returns a Kubernetes Pod that carries the aether managed label
// and a service account, used to seed the fake k8s client.
func validK8sPod(name, namespace string) *corev1.Pod {
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
			ServiceAccountName: "default",
		},
	}
}

// ─── isIgnorablePod ──────────────────────────────────────────────────────────

func TestIsIgnorablePod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *cniv1.CNIPod
		expected bool
	}{
		{
			name: "kube-system namespace is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "coredns",
				Namespace: "kube-system",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1"},
			},
			expected: true,
		},
		{
			name: "aether-system namespace is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "agent",
				Namespace: "aether-system",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1"},
			},
			expected: true,
		},
		{
			name: "nil labels is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    nil,
				Ips:       []string{"10.0.0.1"},
			},
			expected: true,
		},
		{
			name: "missing aether service label is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    map[string]string{"app": "my-app"},
				Ips:       []string{"10.0.0.1"},
			},
			expected: true,
		},
		{
			name: "nil IPs is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       nil,
			},
			expected: true,
		},
		{
			name: "empty IPs slice is ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{},
			},
			expected: true,
		},
		{
			name: "valid pod with all required fields is not ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1"},
			},
			expected: false,
		},
		{
			name: "valid pod with multiple IPs is not ignorable",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "production",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1", "10.0.0.2"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIgnorablePod(tt.pod)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ─── validateAndCheckIgnorable ───────────────────────────────────────────────

func TestValidateAndCheckIgnorable(t *testing.T) {
	tests := []struct {
		name          string
		pod           *cniv1.CNIPod
		wantIgnorable bool
		wantErr       bool
	}{
		{
			name:          "nil pod returns error",
			pod:           nil,
			wantIgnorable: false,
			wantErr:       true,
		},
		{
			name: "ignorable pod returns true without error",
			pod: &cniv1.CNIPod{
				Name:      "coredns",
				Namespace: "kube-system",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1"},
			},
			wantIgnorable: true,
			wantErr:       false,
		},
		{
			name: "valid pod returns false without error",
			pod: &cniv1.CNIPod{
				Name:      "my-pod",
				Namespace: "default",
				Labels:    map[string]string{constants.LabelAetherManaged: "true"},
				Ips:       []string{"10.0.0.1"},
			},
			wantIgnorable: false,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ignorable, err := validateAndCheckIgnorable(tt.pod)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantIgnorable, ignorable)
		})
	}
}

// ─── AddPod ──────────────────────────────────────────────────────────────────

func TestAddPod(t *testing.T) {
	const containerID = "abc123"

	tests := []struct {
		name          string
		setupK8s      func() client.Client
		setupStorage  func() *storage.MockStorage[*cniv1.CNIPod]
		setupRegistry func() *testRegistry
		req           *cniv1.AddPodRequest
		want          *cniv1.AddPodResponse
		wantErr       bool
	}{
		{
			name: "k8s pod not found returns error",
			setupK8s: func() client.Client {
				// empty fake client — no Pod objects present
				return fake.NewClientBuilder().Build()
			},
			setupStorage:  func() *storage.MockStorage[*cniv1.CNIPod] { return storage.NewMockStorage[*cniv1.CNIPod]() },
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.AddPodRequest{
				Pod: &cniv1.CNIPod{
					Name:             "my-pod",
					Namespace:        "default",
					NetworkNamespace: "/proc/1234/ns/net",
					ContainerId:      containerID,
					Ips:              []string{"10.0.0.1"},
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "pod in kube-system is ignored and returns success",
			setupK8s: func() client.Client {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "coredns",
						Namespace: "kube-system",
						Labels:    map[string]string{},
					},
				}
				return fake.NewClientBuilder().WithObjects(pod).Build()
			},
			setupStorage:  func() *storage.MockStorage[*cniv1.CNIPod] { return storage.NewMockStorage[*cniv1.CNIPod]() },
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.AddPodRequest{
				Pod: &cniv1.CNIPod{
					Name:             "coredns",
					Namespace:        "kube-system",
					NetworkNamespace: "/proc/1234/ns/net",
					ContainerId:      containerID,
					Ips:              []string{"10.0.0.1"},
				},
			},
			want:    &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_SUCCESS},
			wantErr: false,
		},
		{
			name: "valid pod with storage failure returns error",
			setupK8s: func() client.Client {
				return fake.NewClientBuilder().WithObjects(validK8sPod("my-pod", "default")).Build()
			},
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.AddResourceFunc = func(_ context.Context, _ types.ContainerID, _ *cniv1.CNIPod) error {
					return errors.New("disk full")
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.AddPodRequest{
				Pod: validCNIPod("my-pod", "default", containerID),
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid pod with registry failure returns error",
			setupK8s: func() client.Client {
				return fake.NewClientBuilder().WithObjects(validK8sPod("my-pod", "default")).Build()
			},
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				return storage.NewMockStorage[*cniv1.CNIPod]()
			},
			setupRegistry: func() *testRegistry {
				return &testRegistry{registerEndpointErr: errors.New("registry unavailable")}
			},
			req: &cniv1.AddPodRequest{
				Pod: validCNIPod("my-pod", "default", containerID),
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid pod with all dependencies succeeds",
			setupK8s: func() client.Client {
				return fake.NewClientBuilder().WithObjects(validK8sPod("my-pod", "default")).Build()
			},
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				return storage.NewMockStorage[*cniv1.CNIPod]()
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.AddPodRequest{
				Pod: validCNIPod("my-pod", "default", containerID),
			},
			want:    &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_SUCCESS},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := cache.NewSnapshotCache("test-node", logr.Discard())
			srv := newTestCNIServer(tt.setupK8s(), tt.setupStorage(), tt.setupRegistry(), sc)

			got, err := srv.AddPod(context.Background(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ─── RemovePod ───────────────────────────────────────────────────────────────

func TestRemovePod(t *testing.T) {
	const containerID = "abc123"

	// A valid stored pod as would have been saved by AddPod.
	storedPod := validCNIPod("my-pod", "default", containerID)

	// An ignorable stored pod (in kube-system, no aether service label).
	ignorableStoredPod := &cniv1.CNIPod{
		Name:             "system-pod",
		Namespace:        "kube-system",
		NetworkNamespace: "/proc/5678/ns/net",
		ContainerId:      containerID,
		Ips:              []string{"10.0.0.2"},
		Labels:           map[string]string{},
	}

	tests := []struct {
		name          string
		setupStorage  func() *storage.MockStorage[*cniv1.CNIPod]
		setupRegistry func() *testRegistry
		req           *cniv1.RemovePodRequest
		want          *cniv1.RemovePodResponse
		wantErr       bool
	}{
		{
			name: "pod not found in storage returns success",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return nil, os.ErrNotExist
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.RemovePodRequest{
				Name:        "missing-pod",
				Namespace:   "default",
				ContainerId: containerID,
			},
			want:    &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_SUCCESS},
			wantErr: false,
		},
		{
			name: "storage get error returns error",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return nil, errors.New("read failure")
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.RemovePodRequest{
				Name:        "my-pod",
				Namespace:   "default",
				ContainerId: containerID,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "stored ignorable pod returns success without unregistering",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return ignorableStoredPod, nil
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.RemovePodRequest{
				Name:        ignorableStoredPod.GetName(),
				Namespace:   ignorableStoredPod.GetNamespace(),
				ContainerId: containerID,
			},
			want:    &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_SUCCESS},
			wantErr: false,
		},
		{
			name: "registry unregister failure returns error",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return storedPod, nil
				}
				return s
			},
			setupRegistry: func() *testRegistry {
				return &testRegistry{unregisterEndpointsErr: errors.New("registry unavailable")}
			},
			req: &cniv1.RemovePodRequest{
				Name:        storedPod.GetName(),
				Namespace:   storedPod.GetNamespace(),
				ContainerId: containerID,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "storage remove failure returns error",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return storedPod, nil
				}
				s.RemoveResourceFunc = func(_ context.Context, _ types.ContainerID) error {
					return errors.New("disk error")
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.RemovePodRequest{
				Name:        storedPod.GetName(),
				Namespace:   storedPod.GetNamespace(),
				ContainerId: containerID,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "valid pod with all dependencies succeeds",
			setupStorage: func() *storage.MockStorage[*cniv1.CNIPod] {
				s := storage.NewMockStorage[*cniv1.CNIPod]()
				s.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
					return storedPod, nil
				}
				return s
			},
			setupRegistry: func() *testRegistry { return &testRegistry{} },
			req: &cniv1.RemovePodRequest{
				Name:        storedPod.GetName(),
				Namespace:   storedPod.GetNamespace(),
				ContainerId: containerID,
			},
			want:    &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_SUCCESS},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := cache.NewSnapshotCache("test-node", logr.Discard())
			k8sClient := fake.NewClientBuilder().Build()
			srv := newTestCNIServer(k8sClient, tt.setupStorage(), tt.setupRegistry(), sc)

			got, err := srv.RemovePod(context.Background(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
