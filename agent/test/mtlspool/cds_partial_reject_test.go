// What the agent may conclude from a cluster ACK that follows a rejected
// cluster update, measured on the pinned proxy (issue #1508).
//
// The node agent's acknowledged SAN-pin gauge is built from what its ACK
// tracker tells of the proxy's cluster answers. Until #1508 an ACK was read as
// "the proxy holds the snapshot this response was built from". These tests
// hold the reading to the two accounts the proxy gives of itself: its admin
// config dump, and the versions it states when it next opens a stream.
package mtlspool

import (
	"maps"
	"testing"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAckAfterARejectedClusterUpdateIsNotAnAckOfTheSnapshot is #1508 end to
// end. The proxy rejects the update of one cluster; then an unrelated cluster
// changes and the proxy acknowledges that.
//
// Measured: go-control-plane does not send the rejected cluster again, so the
// acknowledged response carries the unrelated cluster alone while it names the
// newer snapshot. The proxy goes on running, and stating, the version of the
// rejected cluster it accepted before.
//
// Required of the agent: its reading of that ACK is "the proxy accepted the
// cluster the response carried", and the rejected cluster stays at the version
// the proxy last acknowledged.
func TestAckAfterARejectedClusterUpdateIsNotAnAckOfTheSnapshot(t *testing.T) {
	cp := startCDSControlPlane(t, cdsSocketPath(t), "v1",
		plainCluster("a", 11*time.Second), plainCluster("b", 11*time.Second))
	proxy := startProxyOnCDS(t, cp.socketPath)

	sent := cp.firstResponse(t)
	require.Nil(t, cp.answerTo(t, sent).GetErrorDetail(), "fixture: the proxy accepts both clusters")
	v1 := maps.Clone(cp.versions)

	// v2: the new `a` is a cluster the proxy refuses. `b` is unchanged.
	cp.publish(t, "v2", rejectedCluster("a"), plainCluster("b", 11*time.Second))
	rejected := cp.responseFor(t, "v2")
	assert.Equal(t, []string{"a"}, resourceNames(rejected.resp))
	nack := cp.answerTo(t, rejected)
	require.NotNil(t, nack.GetErrorDetail(), "fixture: the proxy must reject the update of a")
	t.Logf("the proxy's NACK: %s", nack.GetErrorDetail().GetMessage())

	// v3: `a` is still the refused cluster; `b` changes.
	v3Clusters := func() []types.Resource {
		return []types.Resource{rejectedCluster("a"), plainCluster("b", 13*time.Second)}
	}
	cp.publish(t, "v3", v3Clusters()...)
	v3 := maps.Clone(cp.versions)
	accepted := cp.responseFor(t, "v3")
	assert.Equal(t, []string{"b"}, resourceNames(accepted.resp), "the rejected cluster is not sent again")
	assert.Empty(t, accepted.resp.GetRemovedResources())
	require.Nil(t, cp.answerTo(t, accepted).GetErrorDetail(), "the proxy acknowledges the response that carries b alone")

	// What the proxy holds, from its admin config dump: the b of v3, and the a
	// of v1.
	want := map[string]string{"a": v1["a"], "b": v3["b"]}
	require.NotEqual(t, v3["a"], want["a"], "fixture: snapshot v3 has another a than the proxy accepted")
	proxy.await(t, func() string { return "apply the new b" }, func() bool {
		return activeClusterVersions(t, proxy)["b"] == v3["b"]
	})
	running := activeClusterVersions(t, proxy)
	t.Logf("admin config dump: %v", running)
	assert.Equal(t, want, running, "the proxy runs the a it accepted in v1")

	// The agent's reading of the three answers.
	held, tellings := cp.heldByTheAgentsReading()
	t.Logf("the agent's reading after %d tellings: %v", tellings, held)
	assert.Equal(t, 2, tellings, "the ACK of v1 and the ACK of v3's b; the NACK is told to nobody")
	assert.Equal(t, want, held,
		"the ACK of a response that carried b alone must not be read as the proxy holding the a of snapshot v3 (#1508)")

	// And the proxy's own statement of what it holds, from its first request
	// after a restart of the control plane.
	after := cp.restart(t, "v3-again", v3Clusters()...)
	stated := after.firstRequest(t).req.GetInitialResourceVersions()
	t.Logf("the proxy states: %v", stated)
	assert.Equal(t, want, stated)
}

// TestRejectedResponseAppliesItsValidClusters is the limit of "held": Envoy
// applies the valid clusters of a response and then rejects the response as a
// whole. The cluster that was valid runs at its NEW version, and the proxy
// keeps stating the old one. The agent's reading follows what the proxy
// accepted, which is what it states; the admin dump shows the difference.
func TestRejectedResponseAppliesItsValidClusters(t *testing.T) {
	cp := startCDSControlPlane(t, cdsSocketPath(t), "v1", plainCluster("a", 11*time.Second))
	proxy := startProxyOnCDS(t, cp.socketPath)
	require.Nil(t, cp.answerTo(t, cp.firstResponse(t)).GetErrorDetail(), "fixture: the proxy accepts the first cluster")
	v1 := maps.Clone(cp.versions)

	update := func() []types.Resource {
		return []types.Resource{plainCluster("a", 13*time.Second), rejectedCluster("refused")}
	}
	cp.publish(t, "v2", update()...)
	v2 := maps.Clone(cp.versions)
	rejected := cp.responseFor(t, "v2")
	assert.ElementsMatch(t, []string{"a", "refused"}, resourceNames(rejected.resp))
	require.NotNil(t, cp.answerTo(t, rejected).GetErrorDetail(), "fixture: the proxy must reject the update")

	proxy.await(t, func() string { return "apply the valid cluster of the rejected update" }, func() bool {
		return activeClusterVersions(t, proxy)["a"] == v2["a"]
	})
	running := activeClusterVersions(t, proxy)
	t.Logf("admin config dump after the NACK: %v", running)
	assert.Equal(t, map[string]string{"a": v2["a"]}, running, "the proxy RUNS the new a, and no refused cluster")

	held, _ := cp.heldByTheAgentsReading()
	assert.Equal(t, v1, held, "the agent's reading: the a the proxy ACCEPTED")

	after := cp.restart(t, "v2-again", update()...)
	stated := after.firstRequest(t).req.GetInitialResourceVersions()
	t.Logf("the proxy states: %v", stated)
	assert.Equal(t, v1, stated, "and that is what the proxy states")
}
