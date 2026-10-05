package registrar

import (
	"context"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1239: every dependency-set change re-opened the watch with an empty
// resume token, so the registrar resent the whole (filtered) snapshot to every
// agent whose set changed. The token is now kept for the services the cache
// still holds: a shrink resumes with last_version, a growth asks for the added
// services alone (partial_resume), and a token is never presented as covering
// a service the cache does not hold at it.

// requestFor returns the most recent request that asserted exactly services.
func (s *fakeWatchServer) requestFor(services []string) *registrarv1.WatchEndpointsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if slices.Equal(s.requests[i].GetFilter().GetServices(), services) {
			return s.requests[i]
		}
	}
	return nil
}

// TestWatchLoop_DependencySetChangeKeepsTheToken drives the real watch loop
// through the soak's sequence: a filtered watch, a dependency-set shrink (a
// pod deleted), then a growth. The fake answers every stream with the marker
// at version "1".
//
// Red before #1239: the shrink's re-open carried last_version "" (watchLoop
// cleared the token on every filter generation change), which the registrar
// can only answer with a full resend.
func TestWatchLoop_DependencySetChangeKeepsTheToken(t *testing.T) {
	fake, dialOpts := startFakeRegistrar(t, nil)
	r, logs := newLoggingRegistry(t, dialOpts)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))

	await := func(services []string) *registrarv1.WatchEndpointsRequest {
		t.Helper()
		r.SetServiceFilter(services)
		require.Eventuallyf(t, func() bool { return fake.requestFor(services) != nil },
			10*time.Second, time.Millisecond, "filter %v never asserted; logs:\n%s", services, logs)
		return fake.requestFor(services)
	}

	// The startup assert: the unfiltered stream's token covers everything.
	req := await([]string{"default/svc-a", "default/svc-b"})
	assert.Equal(t, "1", req.GetLastVersion(), "narrowing the full watch resumes")
	assert.Nil(t, req.GetPartialResume())

	// The dependency set shrinks: resume, no resend.
	req = await([]string{"default/svc-a"})
	assert.Equal(t, "1", req.GetLastVersion(), "a shrunk dependency set must resume from its token (#1239)")
	assert.Nil(t, req.GetPartialResume())

	// The dependency set grows: the token must NOT go out as last_version (svc-c
	// was never delivered at it); it goes out as a partial resume naming what the
	// cache holds.
	req = await([]string{"default/svc-a", "default/svc-c"})
	assert.Empty(t, req.GetLastVersion(), "a grown filter must never present its token as current")
	require.NotNil(t, req.GetPartialResume())
	assert.Equal(t, "1", req.GetPartialResume().GetVersion())
	assert.Equal(t, []string{"default/svc-a"}, req.GetPartialResume().GetServices())
}

func cacheServices(r *RegistrarRegistry) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, byName := range r.cache {
		for svc := range byName {
			out = append(out, svc)
		}
	}
	slices.Sort(out)
	return out
}

func seedCache(r *RegistrarRegistry, services ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, svc := range services {
		r.upsertLocked(registryv1.Service_PROTOCOL_HTTP, svc, makeEndpoint(fmt.Sprintf("10.0.0.%d", i+1), 8080))
	}
}

// heldAfterStart puts r in the state of a client that completed a stream with
// filter services at token: the cache holds those services and held = them.
func heldAfterStart(t *testing.T, services ...string) *RegistrarRegistry {
	t.Helper()
	r := newTestRegistry()
	r.SetServiceFilter(services)
	seedCache(r, services...)
	r.completeStart(streamOpen{filter: serviceSet(services), lastVersion: resumeToken},
		&registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: resumeToken}, false)
	return r
}

const resumeToken = "7.0123456789abcdef"

func TestResumeFor(t *testing.T) {
	t.Run("no token: a plain resend", func(t *testing.T) {
		r := heldAfterStart(t, "default/a")
		open := r.resumeFor(serviceSet([]string{"default/a"}), "")
		assert.Empty(t, open.lastVersion)
		assert.Nil(t, open.partial)
		assert.True(t, open.noToken)
	})
	t.Run("unchanged filter resumes", func(t *testing.T) {
		r := heldAfterStart(t, "default/a", "default/b")
		open := r.resumeFor(serviceSet([]string{"default/b", "default/a"}), resumeToken)
		assert.Equal(t, resumeToken, open.lastVersion)
		assert.Nil(t, open.partial)
		assert.False(t, open.noToken)
	})
	t.Run("shrink resumes and keeps the held services' endpoints", func(t *testing.T) {
		r := heldAfterStart(t, "default/a", "default/b")
		r.SetServiceFilter([]string{"default/a"})
		open := r.resumeFor(serviceSet([]string{"default/a"}), resumeToken)
		assert.Equal(t, resumeToken, open.lastVersion)
		assert.Nil(t, open.partial)
		assert.Equal(t, []string{"default/a"}, cacheServices(r))
	})
	t.Run("growth asks for the added services only", func(t *testing.T) {
		r := heldAfterStart(t, "default/b", "default/a")
		r.SetServiceFilter([]string{"default/a", "default/b", "default/c"})
		open := r.resumeFor(serviceSet([]string{"default/a", "default/b", "default/c"}), resumeToken)
		assert.Empty(t, open.lastVersion, "never last_version for a grown filter")
		require.NotNil(t, open.partial)
		assert.Equal(t, resumeToken, open.partial.GetVersion())
		assert.Equal(t, []string{"default/a", "default/b"}, open.partial.GetServices(), "sorted, held only")
		assert.True(t, open.noToken)
	})
	t.Run("a service that left and came back within one stream is not held", func(t *testing.T) {
		// The negative: b was purged when it left, and anything the still-live
		// old stream delivered for it after it came back is a fragment. The
		// token must not be presented as covering it.
		r := heldAfterStart(t, "default/a", "default/b")
		r.SetServiceFilter([]string{"default/a"})
		r.SetServiceFilter([]string{"default/a", "default/b"})
		r.applyEvent(context.Background(), &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: "default/b",
			Endpoint:    makeEndpoint("10.0.9.9", 8080),
		})
		open := r.resumeFor(serviceSet([]string{"default/a", "default/b"}), resumeToken)
		assert.Empty(t, open.lastVersion)
		require.NotNil(t, open.partial)
		assert.Equal(t, []string{"default/a"}, open.partial.GetServices())
		assert.Equal(t, []string{"default/a"}, cacheServices(r), "the unheld fragment is dropped before the registrar refills it")
	})
	t.Run("filtered to full watch asks for everything not held", func(t *testing.T) {
		r := heldAfterStart(t, "default/a")
		r.SetServiceFilter(nil)
		open := r.resumeFor(nil, resumeToken)
		assert.Empty(t, open.lastVersion)
		require.NotNil(t, open.partial)
		assert.Equal(t, []string{"default/a"}, open.partial.GetServices())
	})
	t.Run("full watch narrowed resumes", func(t *testing.T) {
		r := newTestRegistry()
		seedCache(r, "default/a", "default/b")
		r.completeStart(streamOpen{lastVersion: resumeToken},
			&registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: resumeToken}, false)
		r.SetServiceFilter([]string{"default/a"})
		open := r.resumeFor(serviceSet([]string{"default/a"}), resumeToken)
		assert.Equal(t, resumeToken, open.lastVersion)
		assert.Equal(t, []string{"default/a"}, cacheServices(r), "the switch from the full watch purges what left")
	})
}

// TestApplyEvent_OutOfScopeIsNotCached: the stream opened under the old filter
// is still delivering when SetServiceFilter purges a service; its events for
// that service must not put it back.
func TestApplyEvent_OutOfScopeIsNotCached(t *testing.T) {
	r := heldAfterStart(t, "default/a", "default/b")
	r.SetServiceFilter([]string{"default/a"})
	for len(r.Changes()) > 0 {
		<-r.Changes()
	}
	r.applyEvent(context.Background(), &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "default/b",
		Endpoint:    makeEndpoint("10.0.9.9", 8080),
	})
	assert.Equal(t, []string{"default/a"}, cacheServices(r))
	assert.Empty(t, r.Changes(), "a dropped event changes nothing")
}

func watchEvent(typ registrarv1.WatchEndpointsResponse_EventType, svc, version string) *registrarv1.WatchEndpointsResponse {
	e := &registrarv1.WatchEndpointsResponse{Type: typ, ServiceName: svc, Version: version}
	if svc != "" {
		e.Endpoint = makeEndpoint("10.0.5.5", 8080)
	}
	return e
}

// TestConsumeStream_ExtendedStart: the registrar honoured the partial resume.
// The added service's endpoints land beside the held ones, the token moves to
// the marker's, and the cache now holds the whole new filter at it.
func TestConsumeStream_ExtendedStart(t *testing.T) {
	r := heldAfterStart(t, "default/a")
	r.SetServiceFilter([]string{"default/a", "default/b"})
	open := r.resumeFor(serviceSet([]string{"default/a", "default/b"}), resumeToken)
	require.NotNil(t, open.partial)

	done := watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, "", "8.0123456789abcdef")
	done.Extended = true
	got, err := r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/b", ""),
		done,
	}, err: io.EOF}, resumeToken, open)
	require.NoError(t, err)
	assert.Equal(t, "8.0123456789abcdef", got)
	assert.Equal(t, []string{"default/a", "default/b"}, cacheServices(r))
	assert.Equal(t, resumeToken, r.resumeFor(serviceSet([]string{"default/a", "default/b"}), resumeToken).lastVersion,
		"the cache now holds both services at the token")
}

// TestConsumeStream_CutExtendedStartKeepsTheToken: the stream dies after part
// of the added services arrived. The old token still names what the held
// services hold, so it survives; the fragment is not held, and the next
// attempt drops it and asks again.
func TestConsumeStream_CutExtendedStartKeepsTheToken(t *testing.T) {
	r := heldAfterStart(t, "default/a")
	r.SetServiceFilter([]string{"default/a", "default/b"})
	filter := serviceSet([]string{"default/a", "default/b"})
	open := r.resumeFor(filter, resumeToken)

	got, err := r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/b", ""),
	}, err: io.EOF}, resumeToken, open)
	require.NoError(t, err)
	assert.Equal(t, resumeToken, got)

	open = r.resumeFor(filter, got)
	assert.Empty(t, open.lastVersion)
	require.NotNil(t, open.partial)
	assert.Equal(t, []string{"default/a"}, open.partial.GetServices())
	assert.Equal(t, []string{"default/a"}, cacheServices(r))
}

// TestConsumeStream_ResendAfterPartialClears: the registrar resent in full
// (a stale token, or one that predates partial_resume). The FULL_SNAPSHOT clears
// the cache and the held set with it.
func TestConsumeStream_ResendAfterPartialClears(t *testing.T) {
	r := heldAfterStart(t, "default/a")
	r.SetServiceFilter([]string{"default/a", "default/b"})
	filter := serviceSet([]string{"default/a", "default/b"})
	open := r.resumeFor(filter, resumeToken)

	// Cut after the first FULL_SNAPSHOT: nothing is held, no token survives.
	got, err := r.consumeStream(context.Background(), &fakeWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/b", ""),
	}, err: io.EOF}, resumeToken, open)
	require.NoError(t, err)
	assert.Empty(t, got)
	r.mu.RLock()
	assert.Empty(t, r.held)
	r.mu.RUnlock()
}

// TestConsumeStream_EmptyResendClearsTheCache: a stream opened without a token
// is resent in full by every registrar unless it says it extended; a resend
// whose filter holds nothing sends no FULL_SNAPSHOT to clear the cache with, so
// the marker must. An extended marker, or a stream that presented a token,
// must leave the cache alone.
func TestConsumeStream_EmptyResendClearsTheCache(t *testing.T) {
	complete := func(extended bool) *registrarv1.WatchEndpointsResponse {
		e := watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, "", "9.0123456789abcdef")
		e.Extended = extended
		return e
	}
	run := func(open streamOpen, marker *registrarv1.WatchEndpointsResponse) []string {
		r := heldAfterStart(t, "default/a")
		_, err := r.consumeStream(context.Background(), &fakeWatchStream{
			events: []*registrarv1.WatchEndpointsResponse{marker}, err: io.EOF,
		}, resumeToken, open)
		require.NoError(t, err)
		return cacheServices(r)
	}
	filter := serviceSet([]string{"default/a"})

	assert.Empty(t, run(streamOpen{filter: filter, noToken: true}, complete(false)), "empty resend")
	assert.Empty(t, run(streamOpen{filter: filter, noToken: true, partial: &registrarv1.PartialResume{Version: resumeToken}}, complete(false)),
		"a partial resume answered by a full resend (stale token, or an older registrar)")
	assert.Equal(t, []string{"default/a"}, run(streamOpen{filter: filter, noToken: true, partial: &registrarv1.PartialResume{Version: resumeToken}}, complete(true)),
		"extended: the held services stay")
	assert.Equal(t, []string{"default/a"}, run(streamOpen{filter: filter, lastVersion: resumeToken}, complete(false)),
		"a presented token: current or renamed, the cache stays")
}
