package mtlspool

// The live gate for issue #1036: a twin the proxy fetched on demand is never
// forgotten by the agent.
//
// Envoy's ODCDS manager keeps ONE subscription per cluster name for the life
// of the process. When the agent removes a twin the proxy fetched on demand --
// its source ServiceAccount's last pod left the node, or its destination left
// the node's dependency set -- Envoy drops the cluster and keeps the subscription. When a
// pod of that ServiceAccount comes back (every Deployment roll), its request
// routes to the twin name again, Envoy logs "already subscribed, skipping",
// sends nothing, and the request 503s NC at the 2 s on_demand timeout -- and
// so does every later one, until the proxy restarts.
//
// The agent keeps such a pair DORMANT (quicdemand.Ledger) and republishes the
// twin the moment the pair is valid again. No request is needed: the
// subscription is still open, so the pushed cluster lands.
//
// Each test has two arms on the same pinned proxy: "dormant" runs the agent's
// ledger; "forget_control" runs the #1035 rule (forget on removal evidence),
// which is the red: 503 at 2 s, and no CDS request ever reaches the agent.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validLocked is the agent's pair validity: the destination is in the node's
// dependency set (every such destination is QUIC-eligible since #979) and the
// source has a pod on the node. Caller holds a.mu.
func (a *odcdsAgent) validLocked(name string) bool {
	svc, source, ok := proxy.ParseQUICClusterName(name, trustDomain)
	return ok && svc == odcdsDestSvc && !a.destGone && !a.awaySources[source]
}

// setSourceAway / setDestinationGone change the pair validity, then reconcile.
func (a *odcdsAgent) setSourceAway(source string, away bool) {
	a.mu.Lock()
	a.awaySources[source] = away
	a.mu.Unlock()
	a.reconcile()
}

func (a *odcdsAgent) setDestinationGone(gone bool) {
	a.mu.Lock()
	a.destGone = gone
	a.mu.Unlock()
	a.reconcile()
}

// reconcile is the agent's snapshot-time pair maintenance: a published twin
// whose pair is no longer valid leaves the snapshot and -- unless this is the
// forget control -- is kept dormant if the proxy holds its subscription
// (Ledger.Retire); a dormant pair that is valid again is republished
// (Ledger.Revive).
func (a *odcdsAgent) reconcile() {
	a.mu.Lock()
	var drop []string
	for _, name := range a.published {
		if a.validLocked(name) {
			continue
		}
		drop = append(drop, name)
		if !a.forget && a.ledger.Retire(name, time.Now()) {
			a.t.Logf("[odcds] %s: pair invalid, kept DORMANT (the proxy holds its subscription)", name)
		} else {
			a.t.Logf("[odcds] %s: pair invalid, forgotten", name)
		}
	}
	var revive []string
	if !a.forget {
		for name := range a.ledger.Revive(a.validLocked) {
			revive = append(revive, name)
		}
	}
	a.mu.Unlock()

	if len(drop) > 0 {
		a.removeTwins(drop)
	}
	slices.Sort(revive)
	for _, name := range revive {
		a.t.Logf("[odcds] %s: dormant pair valid again, republishing with no request", name)
		a.publishTwin(name)
	}
}

// removeTwins drops the named twins and their load assignments from the served
// snapshot in one new version.
func (a *odcdsAgent) removeTwins(names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := map[resourcev3.Type][]types.Resource{}
	for typ, res := range a.resources {
		next[typ] = slices.DeleteFunc(slices.Clone(res), func(r types.Resource) bool {
			return (typ == resourcev3.ClusterType || typ == resourcev3.EndpointType) && slices.Contains(names, cachev3.GetResourceName(r))
		})
	}
	a.version++
	snap, err := cachev3.NewSnapshot(fmt.Sprint(a.version), next)
	if err != nil {
		a.t.Errorf("build snapshot: %v", err)
		return
	}
	if err := a.cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap); err != nil {
		a.t.Errorf("set snapshot: %v", err)
		return
	}
	a.resources = next
	a.published = slices.DeleteFunc(a.published, func(n string) bool { return slices.Contains(names, n) })
	a.t.Logf("[odcds] removed %v (version %d)", names, a.version)
}

// dormantRun drives one arm: A fetches its twin on demand, becomes invalid
// through leave, becomes valid again through ret, and requests.
func dormantRun(t *testing.T, forget bool, leave, ret func(r *odcdsRun)) {
	r := startODCDS(t, true)
	r.agent.mu.Lock()
	r.agent.forget = forget
	r.agent.mu.Unlock()

	at, took := timedCallOnce(t, r.a)
	t.Logf("source-a first request: %s in %s", at, took)
	require.Equal(t, http.StatusOK, at.status, "%s", at)
	require.Equal(t, "HTTP/3.0", at.proto)
	requests, _ := r.agent.snapshotState()
	require.Equal(t, []string{r.twinA}, requests, "precondition: A's twin was fetched on demand")

	leave(r)
	require.Eventually(t, func() bool { return len(r.quicClusters(t)) == 0 }, 10*time.Second, 50*time.Millisecond,
		"the invalid pair's twin must leave the proxy")
	// A real return (a new pod of the ServiceAccount, a destination back in the
	// dependency set) comes seconds later, after Envoy has torn the removed twin down: wait for
	// it to release the twin's EDS name and its certificate's SDS name.
	// Returning inside that teardown races go-control-plane's delta server,
	// which can record a response for a name the proxy is unsubscribing in the
	// same instant and then not re-send it when the re-added twin subscribes
	// again -- a harness artefact, not the behaviour under test.
	require.Eventually(t, func() bool {
		r.agent.mu.Lock()
		defer r.agent.mu.Unlock()
		return r.agent.unsubscribed[r.twinA] && r.agent.unsubscribed[spiffeSourceA]
	}, 10*time.Second, 20*time.Millisecond, "the proxy never released the removed twin's EDS/SDS names")
	t.Logf("pair invalid: proxy holds %v; twin torn down (EDS and SDS released)", r.quicClusters(t))

	ret(r)
	if forget {
		// Give a (wrong) republish every chance to land first.
		time.Sleep(500 * time.Millisecond)
		held := r.quicClusters(t)
		t.Logf("forget control, pair valid again, before any request: proxy holds %v", held)
		assert.Empty(t, held)
		at, took = timedCallOnce(t, r.a)
		t.Logf("source-a after the pair is valid again (forgotten): %s in %s", at, took)
		assert.Equal(t, http.StatusServiceUnavailable, at.status, "a forgotten subscribed twin is stranded: %s", at)
		assert.GreaterOrEqual(t, took, time.Second, "it waits out the on_demand timeout")
		requests, _ = r.agent.snapshotState()
		assert.Equal(t, []string{r.twinA}, requests, "Envoy never re-requests a name it is subscribed to")
		return
	}

	start := time.Now()
	require.Eventually(t, func() bool {
		return slices.Equal([]string{r.twinA}, r.quicClusters(t))
	}, 10*time.Second, 20*time.Millisecond, "the dormant twin must be republished with no request (issue #1036)")
	t.Logf("pair valid again: proxy holds %v after %s, with no request", r.quicClusters(t), time.Since(start))
	requests, _ = r.agent.snapshotState()
	assert.Equal(t, []string{r.twinA}, requests, "no CDS request was needed or sent")

	at, took = timedCallOnce(t, r.a)
	t.Logf("source-a after the pair is valid again: %s in %s", at, took)
	require.NoError(t, at.err)
	require.Equal(t, http.StatusOK, at.status, "%s", at)
	assert.Equal(t, "HTTP/3.0", at.proto)
	assert.Equal(t, spiffeSourceA, at.san)
	assert.Less(t, took, firstRequestODCDSBudget)
}

// TestOnDemandQUICDormantTwinRepublishedWhenSourceReturns: A's ServiceAccount
// leaves the node and comes back (a Deployment roll).
func TestOnDemandQUICDormantTwinRepublishedWhenSourceReturns(t *testing.T) {
	sourceA := proxy.SourceSAKeyFromSpiffeID(spiffeSourceA)
	for _, tc := range []struct {
		name   string
		forget bool
	}{{name: "dormant"}, {name: "forget_control", forget: true}} {
		t.Run(tc.name, func(t *testing.T) {
			dormantRun(t, tc.forget,
				func(r *odcdsRun) { r.agent.setSourceAway(sourceA, true) },
				func(r *odcdsRun) { r.agent.setSourceAway(sourceA, false) })
		})
	}
}

// TestOnDemandQUICDormantTwinRepublishedWhenDestinationReturns: the
// destination leaves the node's dependency set and comes back.
func TestOnDemandQUICDormantTwinRepublishedWhenDestinationReturns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forget bool
	}{{name: "dormant"}, {name: "forget_control", forget: true}} {
		t.Run(tc.name, func(t *testing.T) {
			dormantRun(t, tc.forget,
				func(r *odcdsRun) { r.agent.setDestinationGone(true) },
				func(r *odcdsRun) { r.agent.setDestinationGone(false) })
		})
	}
}
