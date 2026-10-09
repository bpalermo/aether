// What a reconnecting proxy says about the clusters it holds, measured on the
// pinned proxy (issue #1483).
//
// The node agent's acknowledged SAN-pin gauge (aether.agent.xds.acked_tls_clusters)
// is written from the proxy's cluster ACKs. An agent that restarts finds a
// proxy that already holds every cluster, and what that proxy says on its new
// stream is all the new agent process will ever learn about it until a cluster
// changes. These tests pin the three protocol facts the agent's reading of
// that first exchange rests on, against the real Envoy and the real
// go-control-plane server, so a pin bump of either that changes one of them
// fails here rather than in a gauge that quietly stops being true:
//
//  1. The first Cluster request of a stream carries, in
//     initial_resource_versions, the version of every cluster the proxy holds:
//     the per-resource version the previous control plane sent, which for
//     go-control-plane is a hash of the cluster's bytes.
//  2. go-control-plane answers that request even when it has nothing to add
//     and nothing to remove (it answers the first wildcard request of a stream
//     unconditionally), with a response that names the snapshot it compared
//     against, and the proxy ACKs it.
//  3. A proxy never states a version it rejected. A NACKed response leaves its
//     stated versions where they were, for every resource of that response.
package mtlspool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/test/envoybin"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
)

// cdsExchangeWait bounds each wait on one step of a CDS exchange. The proxy
// reconnects on Envoy's default xDS backoff (500 ms base, fully jittered), so
// a step is normally well under a second.
const cdsExchangeWait = 30 * time.Second

// cdsEvent is one Cluster-type message on a control plane's delta stream, in
// the order the server saw it.
type cdsEvent struct {
	stream int64
	req    *discoverygrpc.DeltaDiscoveryRequest  // set for a request
	resp   *discoverygrpc.DeltaDiscoveryResponse // set for a response
}

// cdsControlPlane is one control-plane process as the proxy sees it: a
// go-control-plane snapshot cache served over ADS, the agent's ACK tracker on
// its callbacks, and a record of the Cluster exchange.
type cdsControlPlane struct {
	*adsControlPlane
	versions map[string]string // cluster name -> version of the served snapshot

	mu     sync.Mutex
	events []cdsEvent
	// acked is what the agent's ACK tracker told its observer for the Cluster
	// type: the versions of the snapshots it reads as acknowledged.
	acked []string
}

// startCDSControlPlane serves clusters as snapshot `version` on socketPath.
func startCDSControlPlane(t *testing.T, socketPath, version string, clusters ...types.Resource) *cdsControlPlane {
	t.Helper()

	cp := &cdsControlPlane{}
	snapshot := cp.snapshot(t, version, clusters)
	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	require.NoError(t, cache.SetSnapshot(context.Background(), envoyNodeID, snapshot))

	tracker := ack.NewTracker(slog.New(slog.DiscardHandler))
	tracker.SetAckObserver(func(_ context.Context, typeURL, systemVersion string) {
		if typeURL != resourcev3.ClusterType {
			return
		}
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.acked = append(cp.acked, systemVersion)
	})
	tracked := tracker.Callbacks()

	// The tracker runs first, as it does in the agent, so by the time an event
	// is in the record the tracker has acted on it.
	callbacks := serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: func(stream int64, req *discoverygrpc.DeltaDiscoveryRequest) error {
			err := tracked.OnStreamDeltaRequest(stream, req)
			if req.GetTypeUrl() == resourcev3.ClusterType {
				cp.record(cdsEvent{stream: stream, req: req})
			}
			return err
		},
		StreamDeltaResponseFunc: func(stream int64, req *discoverygrpc.DeltaDiscoveryRequest, resp *discoverygrpc.DeltaDiscoveryResponse) {
			tracked.OnStreamDeltaResponse(stream, req, resp)
			if resp.GetTypeUrl() == resourcev3.ClusterType {
				cp.record(cdsEvent{stream: stream, resp: resp})
			}
		},
		DeltaStreamClosedFunc: tracked.OnDeltaStreamClosed,
	}

	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	gs := grpc.NewServer()
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), cache, callbacks))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	cp.adsControlPlane = &adsControlPlane{socketPath: socketPath, cache: cache, stop: gs.Stop}
	return cp
}

// snapshot builds the snapshot and keeps its per-cluster versions, computed
// the way the cache computes the ones it sends.
func (cp *cdsControlPlane) snapshot(t *testing.T, version string, clusters []types.Resource) *cachev3.Snapshot {
	t.Helper()
	snapshot, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{resourcev3.ClusterType: clusters})
	require.NoError(t, err)
	require.NoError(t, snapshot.ConstructVersionMap())
	cp.mu.Lock()
	cp.versions = maps.Clone(snapshot.GetVersionMap(resourcev3.ClusterType))
	cp.mu.Unlock()
	return snapshot
}

// publish replaces the served clusters under a new snapshot version.
func (cp *cdsControlPlane) publish(t *testing.T, version string, clusters ...types.Resource) {
	t.Helper()
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, cp.snapshot(t, version, clusters)))
}

func (cp *cdsControlPlane) record(ev cdsEvent) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.events = append(cp.events, ev)
}

// ackedVersions is the snapshot versions the ACK tracker has reported so far.
func (cp *cdsControlPlane) ackedVersions() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]string(nil), cp.acked...)
}

// await returns the first recorded event that match accepts, waiting for it.
func (cp *cdsControlPlane) await(t *testing.T, what string, match func(cdsEvent) bool) cdsEvent {
	t.Helper()
	deadline := time.Now().Add(cdsExchangeWait)
	for {
		cp.mu.Lock()
		for _, ev := range cp.events {
			if match(ev) {
				cp.mu.Unlock()
				return ev
			}
		}
		seen := len(cp.events)
		cp.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %s (%d Cluster messages recorded)", what, cdsExchangeWait, seen)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// firstRequest is the first Cluster request of the proxy's stream.
func (cp *cdsControlPlane) firstRequest(t *testing.T) cdsEvent {
	t.Helper()
	return cp.await(t, "first Cluster request", func(ev cdsEvent) bool { return ev.req != nil })
}

// firstResponse is the first Cluster response the server sent.
func (cp *cdsControlPlane) firstResponse(t *testing.T) cdsEvent {
	t.Helper()
	return cp.await(t, "first Cluster response", func(ev cdsEvent) bool { return ev.resp != nil })
}

// answerTo is the request that echoes resp's nonce: its ACK, or its NACK when
// it carries an error detail.
func (cp *cdsControlPlane) answerTo(t *testing.T, resp cdsEvent) *discoverygrpc.DeltaDiscoveryRequest {
	t.Helper()
	nonce := resp.resp.GetNonce()
	return cp.await(t, "answer to the Cluster response with nonce "+nonce, func(ev cdsEvent) bool {
		return ev.req != nil && ev.stream == resp.stream && ev.req.GetResponseNonce() == nonce
	}).req
}

// responseFor is the Cluster response built from the snapshot `version`.
func (cp *cdsControlPlane) responseFor(t *testing.T, version string) cdsEvent {
	t.Helper()
	return cp.await(t, "Cluster response for snapshot "+version, func(ev cdsEvent) bool {
		return ev.resp != nil && ev.resp.GetSystemVersionInfo() == version
	})
}

// restart is an agent restart as the proxy sees it: the control plane stops,
// which drops the ADS stream, and a new one with a new process's state serves
// clusters on the same socket.
func (cp *cdsControlPlane) restart(t *testing.T, version string, clusters ...types.Resource) *cdsControlPlane {
	t.Helper()
	cp.stop()
	return startCDSControlPlane(t, cp.socketPath, version, clusters...)
}

// plainCluster is a STATIC cluster with no transport socket. Nothing here
// handshakes: the tests are about what the proxy says it holds.
func plainCluster(name string, connectTimeout time.Duration) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(connectTimeout),
		LoadAssignment:       staticEndpoint(name, "127.0.0.1:9"),
	}
}

// rejectedCluster is a cluster that is a valid proto, so the control plane
// serves it, and that Envoy refuses: an ORIGINAL_DST cluster must use the
// CLUSTER_PROVIDED load balancer.
func rejectedCluster(name string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_ORIGINAL_DST},
		ConnectTimeout:       durationpb.New(time.Second),
		LbPolicy:             clusterv3.Cluster_ROUND_ROBIN,
	}
}

// startProxyOnCDS runs the pinned proxy against cp with the production ADS
// bootstrap (delta ADS over the agent's Unix socket, `ads: {}` CDS).
func startProxyOnCDS(t *testing.T, socketPath string) *envoyProc {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}
	return launchEnvoyOverADS(t, bin, &adsControlPlane{socketPath: socketPath})
}

// cdsSocketPath is a short path for the control plane's socket: AF_UNIX
// addresses are capped at 107 bytes and a Bazel test tmpdir is long.
func cdsSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aethercds")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "xds.sock")
}

// resourceNames is the names of the resources a response carries.
func resourceNames(resp *discoverygrpc.DeltaDiscoveryResponse) []string {
	names := make([]string, 0, len(resp.GetResources()))
	for _, r := range resp.GetResources() {
		names = append(names, r.GetName())
	}
	return names
}

// TestReconnectingProxyStatesTheClustersItHolds is an agent restart against a
// proxy that is already in sync.
//
// Measured: the proxy's first Cluster request names every cluster it holds
// with the version it was sent; the server, with exactly those clusters in
// its snapshot, answers with an empty response that carries the snapshot's
// version; the proxy ACKs it.
//
// Required of the agent: its ACK tracker reads that acknowledged, empty first
// response as the proxy acknowledging the snapshot it names. Without it the
// restarted agent has no acknowledged pin state until a cluster next changes
// (#1483).
func TestReconnectingProxyStatesTheClustersItHolds(t *testing.T) {
	clusters := func() []types.Resource {
		return []types.Resource{plainCluster("held-a", 11*time.Second), plainCluster("held-b", 11*time.Second)}
	}
	before := startCDSControlPlane(t, cdsSocketPath(t), "before-restart", clusters()...)
	startProxyOnCDS(t, before.socketPath)

	// A proxy with nothing: it states nothing, and is sent everything.
	first := before.firstRequest(t)
	assert.Empty(t, first.req.GetInitialResourceVersions(), "a new proxy holds no cluster to state")
	sent := before.firstResponse(t)
	assert.ElementsMatch(t, []string{"held-a", "held-b"}, resourceNames(sent.resp))
	require.Nil(t, before.answerTo(t, sent).GetErrorDetail(), "fixture: the proxy accepts both clusters")
	require.Equal(t, []string{"before-restart"}, before.ackedVersions())

	after := before.restart(t, "after-restart", clusters()...)

	// Fact 1: the proxy states what it holds, by the versions it was sent.
	// They are content hashes, so a new control plane with the same clusters
	// computes the same ones.
	reconnect := after.firstRequest(t)
	t.Logf("first Cluster request after the restart: subscribe=%v initial_resource_versions=%v",
		reconnect.req.GetResourceNamesSubscribe(), reconnect.req.GetInitialResourceVersions())
	require.Equal(t, before.versions, reconnect.req.GetInitialResourceVersions(),
		"the reconnecting proxy must state every cluster it holds, at the version it was sent")
	require.Equal(t, after.versions, reconnect.req.GetInitialResourceVersions(),
		"fixture: the restarted control plane serves exactly what the proxy holds")
	assert.Empty(t, reconnect.req.GetResponseNonce())

	// Fact 2: nothing is owed, and the server still answers, naming the
	// snapshot it compared the proxy's statement with. The proxy ACKs it.
	answer := after.firstResponse(t)
	assert.Empty(t, answer.resp.GetResources(), "nothing to add: the proxy holds every cluster at this version")
	assert.Empty(t, answer.resp.GetRemovedResources(), "nothing to remove")
	assert.Equal(t, "after-restart", answer.resp.GetSystemVersionInfo())
	require.Nil(t, after.answerTo(t, answer).GetErrorDetail(), "the proxy ACKs the empty first response")

	// The agent's reading: the proxy acknowledged the snapshot that response
	// names, which is the snapshot whose clusters it stated it holds.
	assert.Equal(t, []string{"after-restart"}, after.ackedVersions(),
		"the acknowledged empty first Cluster response of a stream must reach the ACK observer (#1483)")
}

// TestReconnectingProxyStatesNoClusterItRejected is the same restart after the
// proxy rejected a cluster update, which is the state the acknowledged gauge
// exists to show.
//
// Measured: the rejected response changed none of the versions the proxy
// states. The cluster it refused is not stated at all, and the cluster that
// was valid in the same response is stated at its OLD version, although the
// proxy applied the new one (Envoy applies the valid clusters of a response
// it then NACKs as a whole). So a restarted agent that publishes the same
// update is not told "in sync": it sends the update again and is NACKed
// again, and nothing is read as acknowledged.
func TestReconnectingProxyStatesNoClusterItRejected(t *testing.T) {
	before := startCDSControlPlane(t, cdsSocketPath(t), "accepted", plainCluster("held-a", 11*time.Second))
	proxy := startProxyOnCDS(t, before.socketPath)

	sent := before.firstResponse(t)
	require.Nil(t, before.answerTo(t, sent).GetErrorDetail(), "fixture: the proxy accepts the first cluster")
	acceptedVersions := maps.Clone(before.versions)

	// The update the proxy rejects: held-a changed, and a cluster it refuses.
	update := func() []types.Resource {
		return []types.Resource{plainCluster("held-a", 13*time.Second), rejectedCluster("refused")}
	}
	before.publish(t, "rejected", update()...)
	rejected := before.responseFor(t, "rejected")
	assert.ElementsMatch(t, []string{"held-a", "refused"}, resourceNames(rejected.resp))
	nack := before.answerTo(t, rejected)
	require.NotNil(t, nack.GetErrorDetail(), "fixture: the proxy must reject the update")
	t.Logf("the proxy's NACK: %s", nack.GetErrorDetail().GetMessage())
	require.Equal(t, []string{"accepted"}, before.ackedVersions(), "a NACK acknowledges nothing")

	// What the proxy holds is not what it will state: it applied the valid
	// cluster of the response it rejected.
	proxy.await(t, func() string { return "apply the valid cluster of the rejected update" }, func() bool {
		return strings.Contains(dynamicActiveClusters(t, proxy), `"connect_timeout": "13s"`)
	})

	after := before.restart(t, "rejected-again", update()...)

	// Fact 3: no version of the rejected response is stated.
	reconnect := after.firstRequest(t)
	t.Logf("first Cluster request after the restart: initial_resource_versions=%v", reconnect.req.GetInitialResourceVersions())
	require.Equal(t, acceptedVersions, reconnect.req.GetInitialResourceVersions(),
		"after a NACK the proxy must state the versions it had accepted before it, and nothing from the rejected response")
	require.NotEqual(t, after.versions, reconnect.req.GetInitialResourceVersions(),
		"fixture: the restarted control plane serves the update the proxy rejected")

	// So the update is owed again, is sent again, and is rejected again.
	again := after.firstResponse(t)
	assert.ElementsMatch(t, []string{"held-a", "refused"}, resourceNames(again.resp))
	require.NotNil(t, after.answerTo(t, again).GetErrorDetail(), "the proxy rejects the update again")
	assert.Empty(t, after.ackedVersions(), "nothing is acknowledged on a stream whose only cluster response was rejected")
}

// dynamicActiveClusters is the proxy's active xDS-delivered clusters as its
// admin config dump prints them, or "" while the endpoint is not readable.
func dynamicActiveClusters(t *testing.T, e *envoyProc) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://%s/config_dump?resource=dynamic_active_clusters", e.admin))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return string(body)
}
