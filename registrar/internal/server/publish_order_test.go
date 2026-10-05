package server

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1239 review, F2: publications used to hold publishMu only for reading, so
// two of them could broadcast in the opposite order to the one they mutated the
// snapshot in, and on a shared key the watcher's cache ended opposite to the
// snapshot. They are serialized now.

func publishRegister(b *Broadcaster, snap *Snapshot, svc, ip string, hold func()) {
	b.Publish(func() []*registrarv1.WatchEndpointsResponse {
		events := []*registrarv1.WatchEndpointsResponse{{
			Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: svc,
			Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: ep(ip),
		}}
		version, transitions := snap.Apply(events)
		if hold != nil {
			hold()
		}
		return stampVersion(append(events, transitions...), version)
	})
}

func publishRemove(b *Broadcaster, snap *Snapshot, svc string, ips ...string) {
	b.Publish(func() []*registrarv1.WatchEndpointsResponse {
		events, version, transitions := snap.RemoveIPs(svc, ips)
		return stampVersion(append(events, transitions...), version)
	})
}

// appliedCache applies watch events the way the agent does (by service and IP)
// and, at every versioned event, checks the #1203 rule: the receiver holds
// exactly the contents the version names.
type appliedCache struct {
	t       *testing.T
	eps     map[string]map[string]bool
	token   string
	checked int
}

func newAppliedCache(t *testing.T, snap *Snapshot) *appliedCache {
	c := &appliedCache{t: t, eps: map[string]map[string]bool{}}
	for svc, list := range snap.GetAll(registryv1.Service_PROTOCOL_HTTP) {
		for _, e := range list {
			c.set(svc, e.GetIp(), true)
		}
	}
	c.token = snap.Version()
	return c
}

func (c *appliedCache) set(svc, ip string, present bool) {
	if c.eps[svc] == nil {
		c.eps[svc] = map[string]bool{}
	}
	if present {
		c.eps[svc][ip] = true
	} else {
		delete(c.eps[svc], ip)
	}
}

func (c *appliedCache) listing() map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
	svcIPs := map[string][]string{}
	for svc, ips := range c.eps {
		for ip := range ips {
			svcIPs[svc] = append(svcIPs[svc], ip)
		}
	}
	return listing(svcIPs)
}

func (c *appliedCache) apply(e *registrarv1.WatchEndpointsResponse) {
	c.t.Helper()
	switch e.GetType() {
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED:
		c.set(e.GetServiceName(), e.GetEndpoint().GetIp(), true)
	case registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED:
		c.set(e.GetServiceName(), e.GetEndpoint().GetIp(), false)
	}
	if e.GetVersion() == "" {
		return
	}
	c.token = e.GetVersion()
	held := NewSnapshot()
	held.DiffAndReplace(c.listing())
	want, _ := versionContentHash(e.GetVersion())
	c.checked++
	assert.Equal(c.t, want, held.State().ContentHash,
		"version %s reached a cache that does not hold its contents (#1203)", e.GetVersion())
}

// TestPublish_SameKeyPublicationsAreOrdered is the review's interleaving: P1
// (register 10.0.0.2) mutates and is descheduled before its broadcast; P2
// (unregister the same IP) arrives meanwhile. P2 must wait for P1, so the
// watcher sees ADDED then REMOVED and ends matching the snapshot.
//
// Red before the fix: P2 mutated and broadcast in full while P1 was held, the
// watcher saw REMOVED (v2) then ADDED (v1) and kept 10.0.0.2 under v1.
func TestPublish_SameKeyPublicationsAreOrdered(t *testing.T) {
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 5})
	b := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	ch := b.Subscribe("w", []string{"ns/a"})
	cache := newAppliedCache(t, snap)

	mutated, release, p1 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p1)
		publishRegister(b, snap, "ns/a", "10.0.0.2", func() { close(mutated); <-release })
	}()
	<-mutated
	p2 := make(chan struct{})
	go func() {
		defer close(p2)
		publishRemove(b, snap, "ns/a", "10.0.0.2")
	}()
	select {
	case <-p2:
		t.Fatal("a publication completed while another was between its mutation and its broadcast")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-p1
	<-p2

	for len(ch) > 0 {
		cache.apply(<-ch)
	}
	assert.Equal(t, map[string]bool{"10.0.0.1": true}, cache.eps["ns/a"], "the cache matches the snapshot")
	assert.Equal(t, snap.Version(), cache.token)
	_, _, _, resume, _ := snap.WatchStart(cache.token, map[string]struct{}{"ns/a": {}}, nil)
	assert.Equal(t, ResumeCurrent, resume, "and its token is the current version, honestly")
}

// TestPublish_ConcurrentPublicationsKeepEveryVersionHonest drives concurrent
// registers and multi-IP unregisters on a handful of shared keys. At every
// versioned event the watcher must hold exactly the contents that version names
// (no batch interleaved into another, none reordered), and at the end it must
// match the snapshot.
func TestPublish_ConcurrentPublicationsKeepEveryVersionHonest(t *testing.T) {
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 5})
	b := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	ch := b.Subscribe("w", nil)
	cache := newAppliedCache(t, snap)

	// The drain only records, so it keeps up; the check runs afterwards.
	var received []*registrarv1.WatchEndpointsResponse
	drained := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case e := <-ch:
				received = append(received, e)
			case <-stop:
				for len(ch) > 0 {
					received = append(received, <-ch)
				}
				return
			}
		}
	}()

	// Backpressure, so the watcher can never overflow (a force-resync would
	// close its channel and void the check): a publication emits at most 3
	// events (two REMOVED and a SERVICE_REMOVED, or an ADDED and a
	// SERVICE_ADDED), so with every publisher waiting for this much headroom
	// before it publishes, all of them at once cannot fill the buffer whatever
	// the scheduler does. The waits only space the publications out; they
	// still run concurrently, which is what the check is about.
	const publishers = 8
	headroom := publishers * 3
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
	var wg sync.WaitGroup
	for g := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1239))
			for range 200 {
				for len(ch) > defaultChannelBuffer-headroom {
					runtime.Gosched()
				}
				svc := fmt.Sprintf("ns/s%d", rng.IntN(2))
				if rng.IntN(2) == 0 {
					publishRegister(b, snap, svc, ips[rng.IntN(len(ips))], nil)
				} else {
					publishRemove(b, snap, svc, ips[rng.IntN(len(ips))], ips[rng.IntN(len(ips))])
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-drained

	require.Equal(t, 1, b.WatcherCount(), "the watcher never overflowed (backpressure above)")
	for _, e := range received {
		cache.apply(e)
	}
	// 1600 publications, about half of them non-empty: the check must have
	// verified hundreds of versions, never passed having verified none.
	t.Logf("verified %d versioned events", cache.checked)
	require.GreaterOrEqual(t, cache.checked, 400, "versioned events verified")
	held := NewSnapshot()
	held.DiffAndReplace(cache.listing())
	assert.Equal(t, snap.State().ContentHash, held.State().ContentHash, "the cache ends matching the snapshot")
}
