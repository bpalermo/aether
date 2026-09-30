package cache

import (
	"context"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The policy names volume "p", the pods' csi.aether.io volume; the path is
// <root>/<uid>/<file> whatever the volume is called.
const testPolicyUDSPath = testUDSRoot + "/" + testPodUID + "/svc.sock"

// makePolicyPod builds a single-port pod of service "default/echo" with no
// uds-socket annotation, so its delivery comes from the service policy alone.
// Its csi.aether.io volume is "p"; it also declares "uds" (an emptyDir).
func makePolicyPod() *cniv1.CNIPod {
	pod := makeCNIPod("policy-pod", "default", "/proc/910/ns/net")
	pod.Uid = testPodUID
	pod.UdsCsiVolume = "p"
	pod.UdsCsiVolumes = 1
	pod.Volumes = []string{"p", "uds"}
	pod.ServiceAccount = "echo"
	pod.Annotations = map[string]string{aetherannotations.AnnotationEndpointPort: "8080"}
	return pod
}

// appPipePath returns the pipe path of the pod's single app cluster ("" for TCP).
func appPipePath(t *testing.T, c *SnapshotCache, netns string) string {
	t.Helper()
	entry := c.listeners[netns]
	require.Len(t, entry.appClusters, 1)
	cl, ok := entry.appClusters[0].(*clusterv3.Cluster)
	require.True(t, ok)
	return cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe().GetPath()
}

// TestUDSServicePolicy_AppliesWithoutAnnotation verifies the service-scoped policy
// (proposal 034 Phase 1b) drives delivery for a pod that carries no annotation.
func TestUDSServicePolicy_AppliesWithoutAnnotation(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)
	c.SetUDSServicePolicies(map[string]string{"default/echo": "p/svc.sock"})

	pod := makePolicyPod()
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	assert.Equal(t, testPolicyUDSPath, appPipePath(t, c, pod.GetNetworkNamespace()))
	health := c.listeners[pod.GetNetworkNamespace()].healthCluster.(*clusterv3.Cluster)
	assert.Equal(t, testPolicyUDSPath, health.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe().GetPath())
}

// TestUDSServicePolicy_AnnotationWins pins the precedence rule: the pod annotation
// is the most specific declaration and beats the service policy.
func TestUDSServicePolicy_AnnotationWins(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)
	c.SetUDSServicePolicies(map[string]string{"default/echo": "p/svc.sock"})

	// Both declarations name the pod's csi.aether.io volume; the file tells
	// which one won.
	pod := makePolicyPod()
	pod.Annotations[aetherannotations.AnnotationEndpointUDSSocket] = "p/app.sock"
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	assert.Equal(t, testUDSRoot+"/"+testPodUID+"/app.sock", appPipePath(t, c, pod.GetNetworkNamespace()))
}

// TestUDSServicePolicy_OtherServiceUnaffected verifies a policy is scoped to the
// service it targets: a pod of another service keeps TCP loopback delivery.
func TestUDSServicePolicy_OtherServiceUnaffected(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)
	c.SetUDSServicePolicies(map[string]string{"default/other": "p/svc.sock"})

	pod := makePolicyPod()
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	assert.Empty(t, appPipePath(t, c, pod.GetNetworkNamespace()))
}

// TestUDSServicePolicy_ChangeRegeneratesDelivery verifies a policy authored (and
// later removed) after the pod joined regenerates that pod's delivery clusters —
// nothing else would, since the pod itself never changed.
func TestUDSServicePolicy_ChangeRegeneratesDelivery(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)

	pod := makePolicyPod()
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))
	require.Empty(t, appPipePath(t, c, pod.GetNetworkNamespace()), "no policy yet: TCP loopback")

	c.SetUDSServicePolicies(map[string]string{"default/echo": "p/svc.sock"})
	assert.Equal(t, testPolicyUDSPath, appPipePath(t, c, pod.GetNetworkNamespace()))

	// Removing the policy reverts the pod to TCP loopback, with the netns bind back.
	c.SetUDSServicePolicies(nil)
	assert.Empty(t, appPipePath(t, c, pod.GetNetworkNamespace()))
	cl := c.listeners[pod.GetNetworkNamespace()].appClusters[0].(*clusterv3.Cluster)
	addr := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
	assert.Equal(t, "127.0.0.1", addr.GetSocketAddress().GetAddress())
	assert.Equal(t, pod.GetNetworkNamespace(), cl.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath())
}

// TestUDSServicePolicy_UnresolvableFallsBackToTCP verifies a policy naming an
// unusable socket degrades that service's pods to TCP instead of failing them.
func TestUDSServicePolicy_UnresolvableFallsBackToTCP(t *testing.T) {
	c := newTestCache("node-1")
	c.SetUDSCSIRoot(testUDSRoot)
	c.SetUDSServicePolicies(map[string]string{"default/echo": "../escape/app.sock"})

	pod := makePolicyPod()
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	assert.Empty(t, appPipePath(t, c, pod.GetNetworkNamespace()))
}

// TestUDSServicePolicy_EmptyDirVolumeIsNotCSI: a policy naming a volume the pods
// mount as an emptyDir (the pre-039 carrier) is refused with not_csi, not
// resolved to a kubelet path, and the pod keeps TCP delivery.
func TestUDSServicePolicy_EmptyDirVolumeIsNotCSI(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	c.SetUDSCSIRoot(testUDSRoot)
	c.SetUDSServicePolicies(map[string]string{"default/echo": "uds/svc.sock"})

	pod := makePolicyPod()
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	assert.Empty(t, appPipePath(t, c, pod.GetNetworkNamespace()))
	assert.Equal(t, int64(1), counterValue(t, reader, udsResolveFailuresCtr))
	lines := rec.with(udsResolveFailureMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, "not_csi", lines[0].attrs["reason"])
	assert.Equal(t, "endpointpolicy", lines[0].attrs["source"])
}
