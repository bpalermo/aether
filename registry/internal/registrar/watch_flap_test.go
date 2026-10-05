package registrar

import (
	"context"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hookWatchStream returns events[i] after running hooks[i] (if any), then err.
// consumeStream calls Recv only after it has fully processed the previous
// event, so a hook runs exactly between two events.
type hookWatchStream struct {
	registrarv1.RegistrarService_WatchEndpointsClient
	events []*registrarv1.WatchEndpointsResponse
	hooks  map[int]func()
	err    error
	i      int
}

func (h *hookWatchStream) Recv() (*registrarv1.WatchEndpointsResponse, error) {
	if hook := h.hooks[h.i]; hook != nil {
		hook()
	}
	if h.i >= len(h.events) {
		return nil, h.err
	}
	e := h.events[h.i]
	h.i++
	return e, nil
}

func epEvent(typ registrarv1.WatchEndpointsResponse_EventType, svc, ip, version string) *registrarv1.WatchEndpointsResponse {
	return &registrarv1.WatchEndpointsResponse{
		Type: typ, ServiceName: svc, Protocol: registryv1.Service_PROTOCOL_HTTP,
		Endpoint: makeEndpoint(ip, 8080), Version: version,
	}
}

func cacheIPs(r *RegistrarRegistry, svc string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, e := range r.cache[registryv1.Service_PROTOCOL_HTTP][svc] {
		out = append(out, e.GetIp())
	}
	return out
}

// #1239 review, F1: a dependency-set flap (b leaves and re-enters) while a
// stream's INITIAL exchange is still being consumed. SetServiceFilter purges b;
// the stream keeps delivering, so b ends partial. Red before the fix:
// completeStart recomputed held = open.filter ∩ scope, re-admitting b as held,
// the next reconnect presented the token as last_version, and a registrar
// answered "current" with b partial under an accepted token.
func TestConsumeStream_FlapDuringInitialResendIsNotHeld(t *testing.T) {
	const v = "9.0123456789abcdef"
	ab := []string{"default/a", "default/b"}

	r := newTestRegistry()
	r.SetServiceFilter(ab)
	open := r.resumeFor(serviceSet(ab), "") // fresh: a full resend

	stream := &hookWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/a", "10.0.0.1", ""),
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/b", "10.0.1.1", ""),
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/b", "10.0.1.2", ""),
			{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v},
		},
		hooks: map[int]func(){
			// After b's first endpoint: b leaves the dependency set and comes back
			// (grpc-go keeps delivering buffered messages after the cancel).
			2: func() {
				r.SetServiceFilter([]string{"default/a"})
				r.SetServiceFilter(ab)
			},
		},
		// The cancellation SetServiceFilter caused finally surfaces.
		err: status.Error(codes.Canceled, "context canceled"),
	}
	token, err := r.consumeStream(context.Background(), stream, "", open)
	require.NoError(t, err)
	require.Equal(t, v, token)

	// The registrar's contents at v hold b = {10.0.1.1, 10.0.1.2}; the cache lacks 10.0.1.1.
	assert.Equal(t, []string{"10.0.1.2"}, cacheIPs(r, "default/b"), "precondition: b is partial")

	next := r.resumeFor(serviceSet(ab), token)
	assert.Empty(t, next.lastVersion,
		"b is partial: the token must not be presented as last_version (a registrar at %s answers current)", v)
}

// #1239 review, F1: the same flap on a plain resume answered "current" (no events, just
// the marker): a held service is purged wholesale between the open and the
// marker, and the marker re-holds it empty.
func TestConsumeStream_FlapDuringCurrentResumeIsNotHeld(t *testing.T) {
	const v = "9.0123456789abcdef"
	ab := []string{"default/a", "default/b"}

	r := newTestRegistry()
	r.SetServiceFilter(ab)
	r.mu.Lock()
	r.upsertLocked(registryv1.Service_PROTOCOL_HTTP, "default/a", makeEndpoint("10.0.0.1", 8080))
	r.upsertLocked(registryv1.Service_PROTOCOL_HTTP, "default/b", makeEndpoint("10.0.1.1", 8080))
	r.mu.Unlock()
	r.completeStart(streamOpen{filter: serviceSet(ab), lastVersion: v},
		&registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v}, false)

	open := r.resumeFor(serviceSet(ab), v)
	require.Equal(t, v, open.lastVersion)
	stream := &hookWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v},
		},
		hooks: map[int]func(){0: func() {
			r.SetServiceFilter([]string{"default/a"})
			r.SetServiceFilter(ab)
		}},
		err: status.Error(codes.Canceled, "context canceled"),
	}
	token, err := r.consumeStream(context.Background(), stream, v, open)
	require.NoError(t, err)
	assert.Empty(t, cacheIPs(r, "default/b"), "precondition: b was purged")

	next := r.resumeFor(serviceSet(ab), token)
	assert.Empty(t, next.lastVersion, "b is empty in the cache: the token must not be presented as covering it")
}

// #1239 review, P2 (pre-existing, acknowledged in completeStart's comment, exposure
// widened by #1266): a shrink now presents last_version. If that token is stale
// and the new filter's services hold no endpoints at the registrar, the resend
// carries no FULL_SNAPSHOT, nothing clears the cache, and the stale endpoint is
// held under the new token.
func TestConsumeStream_EmptyResendAfterPresentedTokenClears(t *testing.T) {
	const vOld, vNew = "7.0123456789abcdef", "8.fedcba9876543210"
	ab := []string{"default/a", "default/b"}
	r := newTestRegistry()
	r.SetServiceFilter(ab)
	r.mu.Lock()
	r.upsertLocked(registryv1.Service_PROTOCOL_HTTP, "default/a", makeEndpoint("10.0.0.1", 8080))
	r.upsertLocked(registryv1.Service_PROTOCOL_HTTP, "default/b", makeEndpoint("10.0.1.1", 8080))
	r.mu.Unlock()
	r.completeStart(streamOpen{filter: serviceSet(ab), lastVersion: vOld},
		&registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: vOld}, false)

	r.SetServiceFilter([]string{"default/a"})
	open := r.resumeFor(serviceSet([]string{"default/a"}), vOld)
	require.Equal(t, vOld, open.lastVersion)
	// The registrar moved on (a lost its last endpoint while the stream was
	// re-opening): a resend of nothing, then the catalog and the marker.
	stream := &hookWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, ServiceName: "default/b"},
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: vNew},
	}, err: status.Error(codes.Canceled, "x")}
	token, err := r.consumeStream(context.Background(), stream, vOld, open)
	require.NoError(t, err)
	require.Equal(t, vNew, token)
	assert.Empty(t, cacheIPs(r, "default/a"), "a has no endpoints at %s, the cache must not keep 10.0.0.1", vNew)
}

// #1239 review, F1, extension variant: an extended start (the registrar sends
// the added service c) while c flaps. c's first endpoint was purged; the marker
// must not hold c, and the next stream asks for it again.
func TestConsumeStream_FlapDuringExtensionIsNotHeld(t *testing.T) {
	const v = "9.0123456789abcdef"
	ac := []string{"default/a", "default/c"}

	r := heldAfterStart(t, "default/a")
	r.SetServiceFilter(ac)
	open := r.resumeFor(serviceSet(ac), resumeToken)
	require.NotNil(t, open.partial)

	done := &registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v, Extended: true}
	stream := &hookWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/c", "10.0.2.1", ""),
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, "default/c", "10.0.2.2", ""),
			done,
		},
		hooks: map[int]func(){1: func() {
			r.SetServiceFilter([]string{"default/a"})
			r.SetServiceFilter(ac)
		}},
		err: status.Error(codes.Canceled, "context canceled"),
	}
	token, err := r.consumeStream(context.Background(), stream, resumeToken, open)
	require.NoError(t, err)
	require.Equal(t, v, token)
	assert.Equal(t, []string{"10.0.2.2"}, cacheIPs(r, "default/c"), "precondition: c is partial")

	next := r.resumeFor(serviceSet(ac), token)
	assert.Empty(t, next.lastVersion, "c is partial: never presented as current")
	require.NotNil(t, next.partial)
	assert.Equal(t, []string{"default/a"}, next.partial.GetServices())
	assert.Empty(t, cacheIPs(r, "default/c"), "the fragment is dropped before c is asked for again")
}

// #1239 review, P2: the marker tells an empty resend from a resume by its
// content hash. Only a different hash is a resend; current (same version) and
// renamed (same hash, another name) keep the cache, in every version format.
func TestEmptyResend(t *testing.T) {
	marker := func(v string, extended bool) *registrarv1.WatchEndpointsResponse {
		return &registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: v, Extended: extended}
	}
	presented := func(token string) streamOpen { return streamOpen{lastVersion: token} }
	cases := []struct {
		name   string
		open   streamOpen
		marker *registrarv1.WatchEndpointsResponse
		want   bool
	}{
		{"current", presented("7.0123456789abcdef"), marker("7.0123456789abcdef", false), false},
		{"renamed: newer revision", presented("7.0123456789abcdef"), marker("9.0123456789abcdef", false), false},
		{"renamed: dirty to clean", presented("7+0123456789abcdef"), marker("8.0123456789abcdef", false), false},
		{"renamed: hash form", presented("hash:0123456789abcdef"), marker("hash:0123456789abcdef", false), false},
		{"resent: other contents", presented("7.0123456789abcdef"), marker("8.fedcba9876543210", false), true},
		{"resent: other contents, hash form", presented("hash:0123456789abcdef"), marker("hash:fedcba9876543210", false), true},
		{"resent: pre-#1193 counter", presented("41"), marker("42", false), true},
		{"current: pre-#1193 counter", presented("41"), marker("41", false), false},
		{"no token", streamOpen{noToken: true}, marker("8.fedcba9876543210", false), true},
		{"extended", streamOpen{noToken: true}, marker("8.fedcba9876543210", true), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, emptyResend(tc.open, tc.marker))
		})
	}
}

// TestConsumeStream_RenamedResumeKeepsTheCache is P2's negative: a renamed
// answer (same hash) carries no FULL_SNAPSHOT either, and must not clear.
func TestConsumeStream_RenamedResumeKeepsTheCache(t *testing.T) {
	r := heldAfterStart(t, "default/a")
	open := r.resumeFor(serviceSet([]string{"default/a"}), resumeToken)
	require.Equal(t, resumeToken, open.lastVersion)
	stream := &hookWatchStream{events: []*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, ServiceName: "default/a"},
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: "12.0123456789abcdef"},
	}, err: status.Error(codes.Canceled, "x")}
	token, err := r.consumeStream(context.Background(), stream, resumeToken, open)
	require.NoError(t, err)
	assert.Equal(t, "12.0123456789abcdef", token)
	assert.Equal(t, []string{"default/a"}, cacheServices(r))
}
