package cache

import (
	"context"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/udspath"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPodUID  = "11111111-2222-3333-4444-555555555555"
	testUDSRoot = udspath.DefaultCSIRoot
	// The volume name selects the csi.aether.io carrier; it is not in the path.
	testUDSPath = testUDSRoot + "/" + testPodUID + "/app.sock"

	udsResolveFailuresCtr = "aether.agent.uds.resolve_failures"
	udsResolveFailureMsg  = "failed to resolve the pod's UDS socket; falling back to TCP loopback (a UDS-only app stays unpromoted)"
)

// makeUDSPod builds a multi-port pod annotated for UDS delivery whose "uds"
// volume is its csi.aether.io volume, as the CNI server records it on ADD. The
// ports annotation pins the all-ports-one-socket semantic (proposal 034 Phase 1).
func makeUDSPod(uid string) *cniv1.CNIPod {
	pod := makeCNIPod("uds-pod", "default", "/proc/900/ns/net")
	pod.Uid = uid
	pod.UdsCsiVolume = "uds"
	pod.UdsCsiVolumes = 1
	pod.Volumes = []string{"uds", "kube-api-access"}
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

// assertTCPDelivery requires every app cluster of the pod to dial TCP loopback
// in its netns: the safe-degraded fallback.
func assertTCPDelivery(t *testing.T, c *SnapshotCache, pod *cniv1.CNIPod) {
	t.Helper()
	entry := c.listeners[pod.GetNetworkNamespace()]
	require.NotEmpty(t, entry.appClusters)
	for _, r := range entry.appClusters {
		cl := r.(*clusterv3.Cluster)
		addr := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
		assert.Nil(t, addr.GetPipe(), "falls back to TCP loopback")
		assert.Equal(t, "127.0.0.1", addr.GetSocketAddress().GetAddress())
		assert.Equal(t, pod.GetNetworkNamespace(), cl.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
	}
}

// TestAddPod_UDSDelivery verifies an annotated pod with a persisted UID and its
// csi.aether.io volume gets pipe app clusters on EVERY declared port (all
// dialing the same socket under the CSI root) plus a pipe health cluster, none
// of them carrying an upstream bind config.
func TestAddPod_UDSDelivery(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)

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

// TestAddPod_UDSFallsBackToTCP covers every safe-degraded fallback, each with
// the reason the resolve-failure counter reports. None fails the pod add, and
// the kubelet pod-volumes path is never rendered: an emptyDir carrier (the
// pre-039 shape) is not_csi, not a path.
func TestAddPod_UDSFallsBackToTCP(t *testing.T) {
	tests := []struct {
		name   string
		root   string
		mutate func(*cniv1.CNIPod)
		reason udspath.Reason
	}{
		{
			name:   "UDS delivery disabled",
			mutate: func(*cniv1.CNIPod) {},
			reason: udspath.ReasonDisabled,
		},
		{
			name:   "annotated pod without a persisted UID",
			root:   testUDSRoot,
			mutate: func(p *cniv1.CNIPod) { p.Uid = "" },
			reason: udspath.ReasonNoUID,
		},
		{
			name:   "uid does not resolve",
			root:   testUDSRoot,
			mutate: func(p *cniv1.CNIPod) { p.Uid = "../other-pod" },
			reason: udspath.ReasonNoUID,
		},
		{
			// The cut-over's loud failure: the named volume exists, but as an
			// emptyDir (the CNI server recorded no csi.aether.io volume).
			name: "volume is an emptyDir, not csi.aether.io",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.UdsCsiVolume = ""
				p.UdsCsiVolumes = 0
			},
			reason: udspath.ReasonNotCSI,
		},
		{
			name: "volume not declared",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.Annotations[aetherannotations.AnnotationEndpointUDSSocket] = "nope/app.sock"
			},
			reason: udspath.ReasonVolumeNotDeclared,
		},
		{
			// A record replayed from storage written before 039 Phase 2.
			name: "pre-039 record without volume data",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.UdsCsiVolume, p.UdsCsiVolumes, p.Volumes = "", 0, nil
			},
			reason: udspath.ReasonVolumeNotDeclared,
		},
		{
			name: "two csi.aether.io volumes",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.UdsCsiVolume, p.UdsCsiVolumes, p.Volumes = "", 2, []string{"uds", "uds2"}
			},
			reason: udspath.ReasonMultipleCSIVolumes,
		},
		{
			name: "bad socket file",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.Annotations[aetherannotations.AnnotationEndpointUDSSocket] = "uds/.."
			},
			reason: udspath.ReasonBadFile,
		},
		{
			name: "socket path over the AF_UNIX budget",
			root: testUDSRoot,
			mutate: func(p *cniv1.CNIPod) {
				p.Annotations[aetherannotations.AnnotationEndpointUDSSocket] = "uds/" + strings.Repeat("x", udspath.MaxFileLen+1)
			},
			reason: udspath.ReasonPathTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec, reader := newBindingTestCache(t)
			c.SetUDSCSIRoot(tt.root)

			pod := makeUDSPod(testPodUID)
			tt.mutate(pod)
			require.NoError(t, c.AddPod(context.Background(), pod, "example.org"), "a failed resolution must never fail the pod add")

			assertTCPDelivery(t, c, pod)
			assert.Equal(t, int64(1), counterValue(t, reader, udsResolveFailuresCtr), "counted once")
			lines := rec.with(udsResolveFailureMsg)
			require.Len(t, lines, 1)
			assert.Equal(t, string(tt.reason), lines[0].attrs["reason"])
		})
	}
}

// TestUDSResolveFailure_ReportedOncePerPodPerReason pins the bound: a pod that
// keeps failing re-resolves on every delivery-cluster rebuild (policy changes,
// trust-domain folds), and must not log or count on each; a different reason,
// and a failure after the pod was removed and re-added, are reported again.
func TestUDSResolveFailure_ReportedOncePerPodPerReason(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	c.SetUDSCSIRoot(testUDSRoot)

	pod := makeUDSPod(testPodUID)
	pod.UdsCsiVolume, pod.UdsCsiVolumes = "", 0 // emptyDir carrier: not_csi
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))
	for range 3 {
		c.regenerateAllAppDeliveryClusters()
	}
	assert.Equal(t, int64(1), counterValue(t, reader, udsResolveFailuresCtr), "rebuilds do not re-count")
	assert.Len(t, rec.with(udsResolveFailureMsg), 1, "rebuilds do not re-log")

	// The same pod now fails differently (the policy/annotation changed).
	c.listenerMu.Lock()
	entry := c.listeners[pod.GetNetworkNamespace()]
	entry.cniPod.Annotations[aetherannotations.AnnotationEndpointUDSSocket] = "nope/app.sock"
	c.listenerMu.Unlock()
	c.regenerateAllAppDeliveryClusters()
	assert.Equal(t, int64(2), counterValue(t, reader, udsResolveFailuresCtr), "a new reason is reported")

	// Removal forgets the pod; the same failure on a re-add is reported again.
	require.NoError(t, c.RemovePod(context.Background(), pod.GetNetworkNamespace()))
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))
	assert.Equal(t, int64(3), counterValue(t, reader, udsResolveFailuresCtr))
}

// TestUDSResolveFailure_Recovery: a pod fixed in place (its failure cleared by a
// successful resolution) is reported again if it breaks again.
func TestUDSResolveFailure_Recovery(t *testing.T) {
	c, _, reader := newBindingTestCache(t)
	c.SetUDSCSIRoot(testUDSRoot)

	pod := makePolicyPod()
	c.SetUDSServicePolicies(map[string]string{"default/echo": "nope/svc.sock"})
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))
	assert.Equal(t, int64(1), counterValue(t, reader, udsResolveFailuresCtr))

	c.SetUDSServicePolicies(map[string]string{"default/echo": "p/svc.sock"})
	assert.Equal(t, testPolicyUDSPath, appPipePath(t, c, pod.GetNetworkNamespace()))

	c.SetUDSServicePolicies(map[string]string{"default/echo": "nope/svc.sock"})
	assert.Equal(t, int64(2), counterValue(t, reader, udsResolveFailuresCtr))
}

// TestLoadListenersFromStorage_UDSDelivery verifies storage replay resolves the
// socket from the persisted UID and csi.aether.io volume — delivery must not
// depend on the API server being reachable at agent boot.
func TestLoadListenersFromStorage_UDSDelivery(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)

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
// loopback delivery shape byte-for-byte, and reports nothing.
func TestAddPod_TCPPodUnaffected(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	c.SetUDSCSIRoot(testUDSRoot)

	pod := makeCNIPod("tcp-pod", "default", "/proc/901/ns/net")
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	entry := c.listeners[pod.GetNetworkNamespace()]
	require.Len(t, entry.appClusters, 1)
	assertTCPDelivery(t, c, pod)
	assert.Zero(t, counterValue(t, reader, udsResolveFailuresCtr))
	assert.Empty(t, rec.with(udsResolveFailureMsg))
}
