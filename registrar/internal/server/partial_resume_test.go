package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Issue #1239: a dependency-set change re-opens the agent's watch with a new
// filter. A filter that shrank resumes with last_version as any reconnect does;
// one that grew sends partial_resume, and the registrar sends only the added
// services' endpoints when the token names its current contents.

// watchWith opens a watch with req and returns what was sent up to and
// including SNAPSHOT_COMPLETE.
func watchWith(t *testing.T, s *RegistrarServer, req *registrarv1.WatchEndpointsRequest) []*registrarv1.WatchEndpointsResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeWatchServerStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchEndpoints(req, stream) }()
	require.Eventually(t, func() bool {
		return countType(stream.snapshot(), registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE) > 0
	}, 5*time.Second, 10*time.Millisecond, "snapshot-complete marker never sent")
	cancel()
	<-done
	return stream.snapshot()
}

func newPartialTestServer(t *testing.T, m *Metrics) (*RegistrarServer, *Snapshot) {
	t.Helper()
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{
		"ns/a": {"10.0.0.1"},
		"ns/b": {"10.0.1.1", "10.0.1.2"},
		"ns/c": {"10.0.2.1"},
	}), Origin{Revision: 40})
	s := NewRegistrarServer(&flakyRegistry{}, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), m)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)
	return s, snap
}

func filterReq(token string, partial *registrarv1.PartialResume, services ...string) *registrarv1.WatchEndpointsRequest {
	return &registrarv1.WatchEndpointsRequest{
		NodeName:      "n1",
		LastVersion:   token,
		PartialResume: partial,
		Filter:        &registrarv1.ServiceFilter{Services: services},
	}
}

func servicesOf(events []*registrarv1.WatchEndpointsResponse, t registrarv1.WatchEndpointsResponse_EventType) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		if e.GetType() == t {
			out[e.GetServiceName()]++
		}
	}
	return out
}

func marker(t *testing.T, sent []*registrarv1.WatchEndpointsResponse) *registrarv1.WatchEndpointsResponse {
	t.Helper()
	require.NotEmpty(t, sent)
	last := sent[len(sent)-1]
	require.Equal(t, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, last.GetType())
	return last
}

// TestWatchEndpoints_ShrunkFilterResumes: a token earned under the filter
// {a, b} resumes under {a}. The version names the whole snapshot, so the same
// token is current whatever the filter: the server must not need the filter to
// match the one the token was earned under.
func TestWatchEndpoints_ShrunkFilterResumes(t *testing.T) {
	s, snap := newPartialTestServer(t, nil)
	sent := watchWith(t, s, filterReq(snap.Version(), nil, "ns/a"))
	require.Len(t, sent, 1, "a current token under a narrower filter gets the marker alone")
	assert.False(t, marker(t, sent).GetExtended())
}

// TestWatchEndpoints_PartialResumeSendsOnlyTheAddedServices: the filter grew
// from {a} to {a, b}; the client holds a at the current version. It gets b's
// endpoints as ENDPOINT_ADDED (not FULL_SNAPSHOT, which would clear its cache),
// no catalog (the token is current) and an extended marker.
func TestWatchEndpoints_PartialResumeSendsOnlyTheAddedServices(t *testing.T) {
	s, snap := newPartialTestServer(t, nil)
	sent := watchWith(t, s, filterReq("", &registrarv1.PartialResume{Version: snap.Version(), Services: []string{"ns/a"}}, "ns/a", "ns/b"))

	assert.Equal(t, map[string]int{"ns/b": 2}, servicesOf(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED),
		"only the added service's endpoints, never the held one's or an out-of-filter one's")
	assert.Zero(t, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
	assert.Zero(t, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED), "a current token gets no catalog")
	for _, e := range sent[:len(sent)-1] {
		assert.Empty(t, e.GetVersion(), "only the marker may carry the version (#1203)")
	}
	m := marker(t, sent)
	assert.True(t, m.GetExtended())
	assert.Equal(t, snap.Version(), m.GetVersion())
}

// TestWatchEndpoints_PartialResumeRenamed: the token names the current
// contents under an older revision. Extended as above, plus the catalog the
// renamed marker makes the client swap in.
func TestWatchEndpoints_PartialResumeRenamed(t *testing.T) {
	s, snap := newPartialTestServer(t, nil)
	older := "39." + snap.State().ContentHash
	sent := watchWith(t, s, filterReq("", &registrarv1.PartialResume{Version: older, Services: []string{"ns/a"}}, "ns/a", "ns/b"))

	assert.Equal(t, map[string]int{"ns/b": 2}, servicesOf(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED))
	assert.Equal(t, 3, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED), "renamed: the whole catalog")
	m := marker(t, sent)
	assert.True(t, m.GetExtended())
	assert.Equal(t, snap.Version(), m.GetVersion(), "the marker hands the client the current name")
}

// TestWatchEndpoints_PartialResumeStaleTokenIsResent is the negative: a token
// that does not name the current contents must never extend, because the held
// services may be stale too. Full (filtered) snapshot, no extended mark.
func TestWatchEndpoints_PartialResumeStaleTokenIsResent(t *testing.T) {
	s, _ := newPartialTestServer(t, nil)
	stale := "38.0123456789abcdef"
	sent := watchWith(t, s, filterReq("", &registrarv1.PartialResume{Version: stale, Services: []string{"ns/a"}}, "ns/a", "ns/b"))

	assert.Equal(t, map[string]int{"ns/a": 1, "ns/b": 2}, servicesOf(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT),
		"a stale token gets the whole filtered snapshot, held services included")
	assert.Zero(t, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED))
	assert.False(t, marker(t, sent).GetExtended())
}

// TestWatchEndpoints_PartialResumeEdgeCases: an empty partial version is no
// token at all, and a last_version alongside a partial_resume wins (the
// partial is defined only with an empty last_version), so it never extends.
func TestWatchEndpoints_PartialResumeEdgeCases(t *testing.T) {
	t.Run("empty partial version resends", func(t *testing.T) {
		s, _ := newPartialTestServer(t, nil)
		sent := watchWith(t, s, filterReq("", &registrarv1.PartialResume{Services: []string{"ns/a"}}, "ns/a", "ns/b"))
		assert.Equal(t, 3, countType(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT))
		assert.False(t, marker(t, sent).GetExtended())
	})
	t.Run("last_version wins", func(t *testing.T) {
		s, snap := newPartialTestServer(t, nil)
		sent := watchWith(t, s, filterReq(snap.Version(), &registrarv1.PartialResume{Version: snap.Version(), Services: []string{"ns/a"}}, "ns/a", "ns/b"))
		require.Len(t, sent, 1, "an ordinary current resume: marker only")
		assert.False(t, marker(t, sent).GetExtended())
	})
	t.Run("full watch extends with everything not held", func(t *testing.T) {
		s, snap := newPartialTestServer(t, nil)
		sent := watchWith(t, s, &registrarv1.WatchEndpointsRequest{
			NodeName:      "n1",
			PartialResume: &registrarv1.PartialResume{Version: snap.Version(), Services: []string{"ns/a"}},
		})
		assert.Equal(t, map[string]int{"ns/b": 2, "ns/c": 1}, servicesOf(sent, registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED))
		assert.True(t, marker(t, sent).GetExtended())
	})
}

func TestResumeLabel(t *testing.T) {
	assert.Equal(t, "resent", resumeLabel(ResumeResend, false))
	assert.Equal(t, "current", resumeLabel(ResumeCurrent, false))
	assert.Equal(t, "renamed", resumeLabel(ResumeRenamed, false))
	assert.Equal(t, "extended", resumeLabel(ResumeCurrent, true))
	assert.Equal(t, "extended", resumeLabel(ResumeRenamed, true))
}

// TestMetrics_WatchStartsExtended: an extended start is counted as such, never
// as current or renamed.
func TestMetrics_WatchStartsExtended(t *testing.T) {
	m, reader := newTestMetrics(t)
	s, snap := newPartialTestServer(t, m)
	watchWith(t, s, filterReq("", &registrarv1.PartialResume{Version: snap.Version(), Services: []string{"ns/a"}}, "ns/a", "ns/b"))
	watchWith(t, s, filterReq("", &registrarv1.PartialResume{Version: "1.0123456789abcdef"}, "ns/a", "ns/b"))
	watchWith(t, s, filterReq(snap.Version(), nil, "ns/a"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "aether.registrar.watch.starts" {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attrResume)
				got[v.AsString()] += dp.Value
			}
		}
	}
	assert.Equal(t, map[string]int64{"extended": 1, "resent": 1, "current": 1}, got)
}
