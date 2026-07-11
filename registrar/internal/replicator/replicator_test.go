package replicator_test

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bpalermo/aether/registrar/internal/replicator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcetcd "github.com/testcontainers/testcontainers-go/modules/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	localEndpoint string
	peerEndpoint  string
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		// Unit tests (ParsePeers) need no containers.
		os.Exit(m.Run())
	}

	ctx := context.Background()

	localContainer, err := tcetcd.Run(ctx, "gcr.io/etcd-development/etcd:v3.5.21")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to start local etcd container: %v\n", err)
		os.Exit(1)
	}
	peerContainer, err := tcetcd.Run(ctx, "gcr.io/etcd-development/etcd:v3.5.21")
	if err != nil {
		_ = localContainer.Terminate(ctx)
		_, _ = fmt.Fprintf(os.Stderr, "failed to start peer etcd container: %v\n", err)
		os.Exit(1)
	}

	localEndpoint, err = localContainer.ClientEndpoint(ctx)
	if err == nil {
		peerEndpoint, err = peerContainer.ClientEndpoint(ctx)
	}
	if err != nil {
		_ = localContainer.Terminate(ctx)
		_ = peerContainer.Terminate(ctx)
		_, _ = fmt.Fprintf(os.Stderr, "failed to get endpoint: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = localContainer.Terminate(ctx)
	_ = peerContainer.Terminate(ctx)
	os.Exit(code)
}

func TestParsePeers(t *testing.T) {
	t.Run("empty entries disable replication", func(t *testing.T) {
		peers, err := replicator.ParsePeers(nil, "")
		require.NoError(t, err)
		assert.Nil(t, peers)
	})

	t.Run("parses regions and endpoint lists", func(t *testing.T) {
		peers, err := replicator.ParsePeers([]string{
			"region-b=b1:2379,b2:2379",
			"region-c= c1:2379 ",
		}, "region-a")
		require.NoError(t, err)
		require.Len(t, peers, 2)
		assert.Equal(t, replicator.Peer{Region: "region-b", Endpoints: []string{"b1:2379", "b2:2379"}}, peers[0])
		assert.Equal(t, replicator.Peer{Region: "region-c", Endpoints: []string{"c1:2379"}}, peers[1])
	})

	t.Run("requires explicit own region", func(t *testing.T) {
		_, err := replicator.ParsePeers([]string{"region-b=b1:2379"}, "")
		require.ErrorContains(t, err, "--region")
	})

	t.Run("rejects malformed entries", func(t *testing.T) {
		for _, entry := range []string{"region-b", "=b1:2379", "region-b=", "region-b=b1:2379,,b2:2379"} {
			_, err := replicator.ParsePeers([]string{entry}, "region-a")
			assert.Error(t, err, "entry %q", entry)
		}
	})

	t.Run("rejects own region as peer", func(t *testing.T) {
		_, err := replicator.ParsePeers([]string{"region-a=a1:2379"}, "region-a")
		require.ErrorContains(t, err, "own region")
	})

	t.Run("rejects duplicate peer regions", func(t *testing.T) {
		_, err := replicator.ParsePeers([]string{"region-b=b1:2379", "region-b=b2:2379"}, "region-a")
		require.ErrorContains(t, err, "duplicate")
	})
}

// testSource implements replicator.Source over a raw client, standing in for
// the etcd registry's accessors with a per-test partition prefix.
type testSource struct {
	client *clientv3.Client
	prefix string
}

func (s *testSource) Client() *clientv3.Client { return s.client }
func (s *testSource) OwnPrefix() string        { return s.prefix }

// root returns a unique key root per test so tests share the containers.
func root(t *testing.T) string {
	t.Helper()
	return "/" + strings.ReplaceAll(t.Name(), "/", "_")
}

func newClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// startReplicator runs a replicator for ownPrefix→peer and returns a stop
// function that cancels it and waits for Start to return.
func startReplicator(t *testing.T, localCli *clientv3.Client, ownPrefix string, leaseTTLSeconds int64) (stop func()) {
	t.Helper()
	r := &replicator.Replicator{
		Source:          &testSource{client: localCli, prefix: ownPrefix},
		Peers:           []replicator.Peer{{Region: "region-b", Endpoints: []string{peerEndpoint}}},
		Log:             slog.New(slog.DiscardHandler),
		ResyncBackoff:   100 * time.Millisecond,
		LeaseTTLSeconds: leaseTTLSeconds,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, r.Start(ctx))
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

// peerValue fetches a single key's value from the peer ("" when absent).
func peerValue(t *testing.T, peerCli *clientv3.Client, key string) string {
	t.Helper()
	resp, err := peerCli.Get(context.Background(), key)
	require.NoError(t, err)
	if len(resp.Kvs) == 0 {
		return ""
	}
	return string(resp.Kvs[0].Value)
}

func TestReplicator_MirrorsOwnPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	localCli := newClient(t, localEndpoint)
	peerCli := newClient(t, peerEndpoint)

	ownPrefix := root(t) + "/regions/region-a/clusters/c1"
	foreignKey := root(t) + "/regions/region-b/clusters/c9/k"

	// Seed: two own keys + one foreign-region key locally; one stale key on the
	// peer under the own prefix (must be pruned by the initial sync).
	_, err := localCli.Put(ctx, ownPrefix+"/k1", "v1")
	require.NoError(t, err)
	_, err = localCli.Put(ctx, ownPrefix+"/k2", "v2")
	require.NoError(t, err)
	_, err = localCli.Put(ctx, foreignKey, "not-ours")
	require.NoError(t, err)
	_, err = peerCli.Put(ctx, ownPrefix+"/stale", "gone-local")
	require.NoError(t, err)

	startReplicator(t, localCli, ownPrefix, 0)

	// Initial sync: own keys arrive, the stale peer key is pruned.
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == "v1" &&
			peerValue(t, peerCli, ownPrefix+"/k2") == "v2" &&
			peerValue(t, peerCli, ownPrefix+"/stale") == ""
	}, 15*time.Second, 50*time.Millisecond, "initial sync did not converge")

	// The foreign-region key is outside the own partition and never mirrored.
	assert.Empty(t, peerValue(t, peerCli, foreignKey))

	// Live mirroring: put new, update existing, delete existing.
	_, err = localCli.Put(ctx, ownPrefix+"/k3", "v3")
	require.NoError(t, err)
	_, err = localCli.Put(ctx, ownPrefix+"/k2", "v2-updated")
	require.NoError(t, err)
	_, err = localCli.Delete(ctx, ownPrefix+"/k1")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k3") == "v3" &&
			peerValue(t, peerCli, ownPrefix+"/k2") == "v2-updated" &&
			peerValue(t, peerCli, ownPrefix+"/k1") == ""
	}, 15*time.Second, 50*time.Millisecond, "live mirror did not converge")
}

func TestReplicator_ResumesAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	localCli := newClient(t, localEndpoint)
	peerCli := newClient(t, peerEndpoint)

	ownPrefix := root(t) + "/regions/region-a/clusters/c1"
	_, err := localCli.Put(ctx, ownPrefix+"/k1", "v1")
	require.NoError(t, err)
	_, err = localCli.Put(ctx, ownPrefix+"/k2", "v2")
	require.NoError(t, err)

	stop := startReplicator(t, localCli, ownPrefix, 0)
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == "v1" &&
			peerValue(t, peerCli, ownPrefix+"/k2") == "v2"
	}, 15*time.Second, 50*time.Millisecond, "initial sync did not converge")
	stop()

	// Mutate locally while no replicator runs: the restart's reconcile-sync
	// (not the watch) must converge both the delete and the add.
	_, err = localCli.Delete(ctx, ownPrefix+"/k1")
	require.NoError(t, err)
	_, err = localCli.Put(ctx, ownPrefix+"/k3", "v3")
	require.NoError(t, err)

	startReplicator(t, localCli, ownPrefix, 0)
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == "" &&
			peerValue(t, peerCli, ownPrefix+"/k3") == "v3"
	}, 15*time.Second, 50*time.Millisecond, "resync after restart did not converge")
}

func TestReplicator_LeaseExpiresOnOriginDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	localCli := newClient(t, localEndpoint)
	peerCli := newClient(t, peerEndpoint)

	ownPrefix := root(t) + "/regions/region-a/clusters/c1"
	_, err := localCli.Put(ctx, ownPrefix+"/k1", "v1")
	require.NoError(t, err)

	stop := startReplicator(t, localCli, ownPrefix, 3)
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == "v1"
	}, 15*time.Second, 50*time.Millisecond, "initial sync did not converge")

	// Origin dies (keepalive stops, no revoke): the mirrored key must expire
	// with the lease TTL — whole-region failover cleanup on the peer.
	stop()
	assert.Equal(t, "v1", peerValue(t, peerCli, ownPrefix+"/k1"), "mirror must not vanish at shutdown (no revoke)")
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == ""
	}, 20*time.Second, 100*time.Millisecond, "mirrored key did not expire after origin death")
}

func TestReplicator_LeaseHandoffKeepsMirrorAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	localCli := newClient(t, localEndpoint)
	peerCli := newClient(t, peerEndpoint)

	ownPrefix := root(t) + "/regions/region-a/clusters/c1"
	_, err := localCli.Put(ctx, ownPrefix+"/k1", "v1")
	require.NoError(t, err)

	stop := startReplicator(t, localCli, ownPrefix, 10)
	require.Eventually(t, func() bool {
		return peerValue(t, peerCli, ownPrefix+"/k1") == "v1"
	}, 15*time.Second, 50*time.Millisecond, "initial sync did not converge")

	// Leader handoff: the successor re-puts the (value-unchanged) key under
	// its fresh lease. If the sync skipped it on value equality alone, it
	// would stay on the predecessor's lease and expire below.
	stop()
	startReplicator(t, localCli, ownPrefix, 10)

	// Watch through the predecessor's TTL expiry (plus slack): the mirrored
	// key must never disappear across the handoff.
	deadline := time.Now().Add(14 * time.Second)
	for time.Now().Before(deadline) {
		require.Equal(t, "v1", peerValue(t, peerCli, ownPrefix+"/k1"), "mirror dropped during leader handoff")
		time.Sleep(200 * time.Millisecond)
	}
}
