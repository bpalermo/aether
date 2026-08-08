package cache

import (
	"context"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPodUID     = "11111111-2222-3333-4444-555555555555"
	testKubeletDir = "/var/lib/kubelet/pods"
	testUDSPath    = testKubeletDir + "/" + testPodUID + "/volumes/kubernetes.io~empty-dir/uds/app.sock"
)

// makeUDSPod builds a multi-port pod annotated for UDS delivery. The ports
// annotation pins the all-ports-one-socket semantic (proposal 034 Phase 1).
func makeUDSPod(uid string) *cniv1.CNIPod {
	pod := makeCNIPod("uds-pod", "default", "/proc/900/ns/net")
	pod.Uid = uid
	pod.Annotations = map[string]string{
		aetherannotations.AnnotationEndpointPort:      "8080",
		aetherannotations.AnnotationEndpointPorts:     "8080,9090",
		aetherannotations.AnnotationEndpointUDSSocket: "uds/app.sock",
	}
	return pod
}

// pipePaths returns the endpoint pipe path of each cluster resource ("" for a
// TCP endpoint), keyed by cluster name.
func pipePaths(t *testing.T, resources []types.Resource) map[string]string {
	t.Helper()
	out := make(map[string]string, len(resources))
	for _, r := range resources {
		c, ok := r.(*clusterv3.Cluster)
		require.True(t, ok, "cluster resource")
		out[c.GetName()] = c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe().GetPath()
	}
	return out
}

// TestAddPod_UDSDelivery verifies an annotated pod with a persisted UID gets
// pipe app clusters on EVERY declared port (all dialing the same socket) plus a
// pipe health cluster, none of them carrying an upstream bind config.
func TestAddPod_UDSDelivery(t *testing.T) {
	c := newTestCache("node-1")
	c.SetKubeletPodsDir(testKubeletDir)

	pod := makeUDSPod(testPodUID)
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	entry := c.listeners[pod.GetNetworkNamespace()]
	require.Len(t, entry.appClusters, 2, "one app cluster per declared port")
	assert.Equal(t, map[string]string{
		"app_uds-pod_8080": testUDSPath,
		"app_uds-pod_9090": testUDSPath,
	}, pipePaths(t, entry.appClusters), "every declared port dials the pod's one socket")
	for _, r := range entry.appClusters {
		assert.Nil(t, r.(*clusterv3.Cluster).GetUpstreamBindConfig(), "pipe upstreams carry no netns bind")
	}

	health := entry.healthCluster.(*clusterv3.Cluster)
	assert.Equal(t, testUDSPath, health.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe().GetPath())
	assert.Nil(t, health.GetUpstreamBindConfig())
}

// TestAddPod_UDSFallsBackToTCP covers the two safe-degraded fallbacks: a stored
// record without a pod UID (written before the UID was persisted) and the
// operator gate (--kubelet-pods-dir empty). Both keep TCP loopback delivery
// rather than failing the pod add.
func TestAddPod_UDSFallsBackToTCP(t *testing.T) {
	tests := []struct {
		name           string
		kubeletPodsDir string
		uid            string
	}{
		{name: "annotated pod without a persisted UID", kubeletPodsDir: testKubeletDir, uid: ""},
		{name: "UDS delivery disabled", kubeletPodsDir: "", uid: testPodUID},
		{name: "annotation does not resolve", kubeletPodsDir: testKubeletDir, uid: "../other-pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			c.SetKubeletPodsDir(tt.kubeletPodsDir)

			pod := makeUDSPod(tt.uid)
			require.NoError(t, c.AddPod(context.Background(), pod, "example.org"), "a failed resolution must never fail the pod add")

			entry := c.listeners[pod.GetNetworkNamespace()]
			require.Len(t, entry.appClusters, 2)
			for _, r := range entry.appClusters {
				cl := r.(*clusterv3.Cluster)
				addr := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
				assert.Nil(t, addr.GetPipe(), "falls back to TCP loopback")
				assert.Equal(t, "127.0.0.1", addr.GetSocketAddress().GetAddress())
				assert.Equal(t, pod.GetNetworkNamespace(), cl.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
			}
		})
	}
}

// TestLoadListenersFromStorage_UDSDelivery verifies storage replay resolves the
// socket from the persisted UID — delivery must not depend on the API server
// being reachable at agent boot.
func TestLoadListenersFromStorage_UDSDelivery(t *testing.T) {
	c := newTestCache("node-1")
	c.SetKubeletPodsDir(testKubeletDir)

	pod := makeUDSPod(testPodUID)
	seedListeners(c, pod)

	entry := c.listeners[pod.GetNetworkNamespace()]
	require.Len(t, entry.appClusters, 2)
	assert.Equal(t, map[string]string{
		"app_uds-pod_8080": testUDSPath,
		"app_uds-pod_9090": testUDSPath,
	}, pipePaths(t, entry.appClusters))
}

// TestAddPod_TCPPodUnaffected pins that a pod without the annotation keeps the
// loopback delivery shape byte-for-byte.
func TestAddPod_TCPPodUnaffected(t *testing.T) {
	c := newTestCache("node-1")
	c.SetKubeletPodsDir(testKubeletDir)

	pod := makeCNIPod("tcp-pod", "default", "/proc/901/ns/net")
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	entry := c.listeners[pod.GetNetworkNamespace()]
	require.Len(t, entry.appClusters, 1)
	cl := entry.appClusters[0].(*clusterv3.Cluster)
	addr := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
	assert.Nil(t, addr.GetPipe())
	assert.Equal(t, "127.0.0.1", addr.GetSocketAddress().GetAddress())
	assert.Equal(t, pod.GetNetworkNamespace(), cl.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
}
