package registrar

import (
	"context"
	"io"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Issue #1334 (observed as #1324): a watch stream that ends before the
// SNAPSHOT_COMPLETE of its initial exchange, while the client holds no resume
// token, is followed by a second full resend. Nothing on the agent said so:
// watch_token_drops counts only a non-empty token given up after a completed
// start.

const (
	abandonedMetric = "aether.agent.registry.watch_resends_abandoned"
	abandonedLog    = "watch stream ended before its first SNAPSHOT_COMPLETE with no resume token"
)

// counterByReason returns a counter's value per "reason" attribute (nil when
// the instrument has recorded nothing).
func counterByReason(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var out map[string]int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %s is %T, want Sum[int64]", name, m.Data)
			for _, dp := range sum.DataPoints {
				if out == nil {
					out = map[string]int64{}
				}
				reason, _ := dp.Attributes.Value("reason")
				out[reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestWatchLoop_FilterChangeBeforeFirstSnapshotComplete drives the real watch
// loop against modelRegistrar through the #1324 sequence: the agent's first
// stream is opened with no token, and the dependency set changes while the
// registrar is still sending that stream its snapshot. The model holds stream
// 1 after its FULL_SNAPSHOT events and before the marker, which is where the
// change has to land.
//
// It pins the mechanism (the second stream carries no token either, so the
// registrar counts two "resent" for one agent start), that the outcome is
// correct anyway (the cache ends equal to a full filtered resend, and readiness
// is not announced on the abandoned half), and that it is now counted and
// logged. The contrast cases put the same change after the marker, where the
// token exists and the re-open is an extension.
func TestWatchLoop_FilterChangeBeforeFirstSnapshotComplete(t *testing.T) {
	c0 := map[string][]string{
		"default/a": {"10.0.0.1"},
		"default/b": {"10.0.1.1"},
		"default/c": {"10.0.2.1"},
	}
	a, ab, abc := []string{"default/a"}, []string{"default/a", "default/b"}, []string{"default/a", "default/b", "default/c"}

	cases := []struct {
		name string
		// start is the filter asserted before Initialize, as the xDS PreListen
		// does ahead of the identity hold (set = false: none, the full watch).
		start []string
		set   bool
		// beforeMarker lands the change inside stream 1's initial exchange.
		beforeMarker bool
		change       []string
		outcomes     []string
		abandoned    map[string]int64
	}{
		{
			name: "a growth before the first SNAPSHOT_COMPLETE: a second tokenless stream",
			set:  true, start: a, beforeMarker: true, change: ab,
			outcomes:  []string{"resent", "resent"},
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "a shrink before the first SNAPSHOT_COMPLETE: the same",
			set:  true, start: ab, beforeMarker: true, change: a,
			outcomes:  []string{"resent", "resent"},
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "an empty dependency set that gains its first service before the marker",
			set:  true, start: []string{}, beforeMarker: true, change: a,
			outcomes:  []string{"resent", "resent"},
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name:         "the full watch scoped before the marker",
			beforeMarker: true, change: a,
			outcomes:  []string{"resent", "resent"},
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "contrast: the same growth after the marker is an extension",
			set:  true, start: a, change: ab,
			outcomes: []string{"resent", "extended"},
		},
		{
			name: "contrast: an empty dependency set that grows after the marker is an extension",
			set:  true, start: []string{}, change: a,
			outcomes: []string{"resent", "extended"},
		},
		{
			name: "contrast: a shrink after the marker resumes",
			set:  true, start: ab, change: a,
			outcomes: []string{"resent", "current"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModelRegistrar(10, c0)
			reached := make(chan struct{})
			if c.beforeMarker {
				m.beforeMarker = func(ctx context.Context, n int) {
					if n == 1 {
						close(reached)
						<-ctx.Done() // until the client cancels the stream
					}
				}
			}
			r, logs := newLoggingRegistry(t, startModelRegistrar(t, m))
			var reader *sdkmetric.ManualReader
			r.metrics, reader = newTestClientMetrics(t)
			if c.set {
				r.SetServiceFilter(c.start)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			require.NoError(t, r.Initialize(ctx))
			defer func() { _ = r.Close() }()

			if c.beforeMarker {
				select {
				case <-reached:
				case <-time.After(10 * time.Second):
					t.Fatalf("stream 1 never reached its marker; logs:\n%s", logs)
				}
				select {
				case <-r.ready:
					t.Fatal("the cache was announced complete before any SNAPSHOT_COMPLETE")
				default:
				}
			} else {
				require.NoError(t, r.WaitReady(ctx))
				require.Equal(t, m.view(serviceSet(c.start)), cacheView(r))
			}

			r.SetServiceFilter(c.change)

			require.Eventually(t, func() bool { return len(m.outcomeLog()) == 2 }, 10*time.Second, time.Millisecond,
				"no second stream; logs:\n%s", logs)
			require.NoError(t, r.WaitReady(ctx))
			want := m.view(serviceSet(c.change))
			assert.Eventuallyf(t, func() bool { return assert.ObjectsAreEqual(want, cacheView(r)) },
				2*time.Second, time.Millisecond, "the cache is not the registrar's contents: outcomes %v, cache %v, want %v; logs:\n%s",
				m.outcomeLog(), cacheView(r), want, logs)
			assert.Equal(t, c.outcomes, m.outcomeLog())

			second := m.requestLog()[1]
			assert.ElementsMatch(t, c.change, second.GetFilter().GetServices(), "the second stream asserts the changed filter")
			if c.beforeMarker {
				assert.Empty(t, second.GetLastVersion(), "no token was ever earned")
				assert.Nil(t, second.GetPartialResume(), "and none is offered as a partial resume")
				assert.Contains(t, logs.String(), abandonedLog)
				assert.Contains(t, logs.String(), `"reason":"filter_change"`)
			} else {
				assert.NotContains(t, logs.String(), abandonedLog)
			}
			assert.Equal(t, c.abandoned, counterByReason(t, reader, abandonedMetric))
			assert.Nil(t, counterByReason(t, reader, "aether.agent.registry.watch_token_drops"),
				"an abandoned resend is not a token drop")

			// A filter that legitimately changes later still works, and resumes
			// from the token the completed stream earned.
			r.SetServiceFilter(abc)
			require.Eventually(t, func() bool { return len(m.outcomeLog()) == 3 }, 10*time.Second, time.Millisecond)
			assert.Eventuallyf(t, func() bool { return assert.ObjectsAreEqual(m.view(nil), cacheView(r)) },
				2*time.Second, time.Millisecond, "after the later growth: cache %v, want %v; logs:\n%s", cacheView(r), m.view(nil), logs)
			assert.Equal(t, "extended", m.outcomeLog()[2])
			assert.Equal(t, c.abandoned, counterByReason(t, reader, abandonedMetric), "the later change abandons nothing")
		})
	}
}

// TestConsumeStream_AbandonedResend pins which stream ends are an abandoned
// resend: before the initial exchange's marker AND leaving no token. What ended
// the stream is the reason; our own shutdown is not counted.
func TestConsumeStream_AbandonedResend(t *testing.T) {
	const v2 = "8.0123456789abcdef"
	type ev = *registrarv1.WatchEndpointsResponse
	full := func(svc, ip string) ev {
		return epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, svc, ip, "")
	}
	added := func(svc, ip string) ev {
		return epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, svc, ip, "")
	}
	marker := func(version string, extended bool) ev {
		return &registrarv1.WatchEndpointsResponse{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, Version: version, Extended: extended}
	}
	a, ab := []string{"default/a"}, []string{"default/a", "default/b"}

	cases := []struct {
		name string
		// token is the token the cache holds for held when the stream opens
		// ("" = none: the process's first stream).
		token  string
		held   []string
		filter []string
		events []ev
		end    error
		// shutdown cancels the context before the stream ends.
		shutdown  bool
		wantToken string
		wantFail  bool
		abandoned map[string]int64
		drops     map[string]int64
	}{
		{
			name: "tokenless, cancelled by a filter change mid-resend", filter: ab,
			events: []ev{full("default/a", "10.0.0.1")}, end: status.Error(codes.Canceled, "context canceled"),
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "tokenless, cancelled before any event", filter: ab,
			end:       status.Error(codes.Canceled, "context canceled"),
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "tokenless with an empty dependency set", filter: []string{},
			end:       status.Error(codes.Canceled, "context canceled"),
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name: "tokenless, clean end of stream", filter: ab,
			events: []ev{full("default/a", "10.0.0.1")}, end: io.EOF,
			abandoned: map[string]int64{streamEndEOF: 1},
		},
		{
			name: "tokenless, server drain", filter: ab,
			events:    []ev{full("default/a", "10.0.0.1")},
			end:       status.Error(codes.Unavailable, "closing transport due to: EOF, "+goawayNoErrorDetail),
			abandoned: map[string]int64{streamEndServerDrain: 1},
		},
		{
			name: "tokenless, forced resync", filter: ab,
			end:       status.Error(codes.DataLoss, "watch stream overflowed"),
			abandoned: map[string]int64{streamEndForcedResync: 1},
		},
		{
			name: "tokenless, stream failure", filter: ab,
			events: []ev{full("default/a", "10.0.0.1")}, end: status.Error(codes.Unavailable, "connection reset"),
			wantFail:  true,
			abandoned: map[string]int64{streamEndError: 1},
		},
		{
			name: "tokenless, our own shutdown is not counted", filter: ab,
			events: []ev{full("default/a", "10.0.0.1")}, end: status.Error(codes.Canceled, "context canceled"),
			shutdown: true,
		},
		{
			name:  "a presented token the registrar resends past is dropped at the first FULL_SNAPSHOT (#1203): abandoned too",
			token: resumeToken, held: ab, filter: ab,
			events: []ev{full("default/a", "10.0.0.9")}, end: status.Error(codes.Canceled, "context canceled"),
			abandoned: map[string]int64{streamEndFilterChange: 1},
		},
		{
			name:  "a presented token, cut before the marker with nothing resent: the token stands",
			token: resumeToken, held: ab, filter: ab,
			end:       status.Error(codes.Canceled, "context canceled"),
			wantToken: resumeToken,
		},
		{
			name:  "a partial resume cut inside its extension: the token stands",
			token: resumeToken, held: a, filter: ab,
			events: []ev{added("default/b", "10.0.1.1")}, end: io.EOF,
			wantToken: resumeToken,
		},
		{
			name: "tokenless, completed: nothing abandoned", filter: ab,
			events: []ev{full("default/a", "10.0.0.1"), marker(v2, false)}, end: io.EOF,
			wantToken: v2,
		},
		{
			name: "tokenless, completed, then cut mid-batch: a token drop, not an abandoned resend", filter: ab,
			events: []ev{full("default/a", "10.0.0.1"), marker(v2, false), added("default/a", "10.0.0.2")}, end: io.EOF,
			drops: map[string]int64{tokenDropMidBatch: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r *RegistrarRegistry
			if c.token != "" {
				r = heldAfterStart(t, c.held...)
			} else {
				r = newTestRegistry()
			}
			r.SetServiceFilter(c.filter)
			var reader *sdkmetric.ManualReader
			r.metrics, reader = newTestClientMetrics(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.shutdown {
				cancel()
			}
			open := r.resumeFor(serviceSet(c.filter), c.token)
			got, failure := r.consumeStream(ctx, &fakeWatchStream{events: c.events, err: c.end}, c.token, open)

			assert.Equal(t, c.wantToken, got)
			assert.Equal(t, c.wantFail, failure != nil, "failure = %v", failure)
			assert.Equal(t, c.abandoned, counterByReason(t, reader, abandonedMetric))
			assert.Equal(t, c.drops, counterByReason(t, reader, "aether.agent.registry.watch_token_drops"))
			if c.abandoned != nil {
				next := r.resumeFor(serviceSet(c.filter), got)
				assert.True(t, next.noToken, "the stream after an abandoned resend carries no token")
				assert.Nil(t, next.partial)
			}
		})
	}
}

// TestConsumeStream_AbandonedResendLog pins the INFO line and what it carries:
// enough to tell, with debug logging off, how far the abandoned stream got and
// which filter change superseded it.
func TestConsumeStream_AbandonedResendLog(t *testing.T) {
	r, logs := newLoggingRegistry(t, nil)
	r.SetServiceFilter([]string{"default/a"})
	open := r.resumeFor(serviceSet([]string{"default/a"}), "")
	// The dependency set grows while the stream is being read.
	r.SetServiceFilter([]string{"default/a", "default/b", "default/c"})

	got, failure := r.consumeStream(context.Background(), &fakeWatchStream{
		events: []*registrarv1.WatchEndpointsResponse{
			epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/a", "10.0.0.1", ""),
		},
		err: status.Error(codes.Canceled, "context canceled"),
	}, "", open)
	require.NoError(t, failure)
	require.Empty(t, got)

	out := logs.String()
	require.Contains(t, out, abandonedLog)
	for _, field := range []string{
		`"level":"INFO"`, `"reason":"filter_change"`, `"eventsReceived":1`, `"tokenPresented":false`,
		`"filtered":true`, `"filterServices":1`, `"nextFiltered":true`, `"nextFilterServices":3`,
	} {
		assert.Contains(t, out, field)
	}
}

// TestConsumeStream_AbandonedResendShutdown: a shutdown is never an abandoned
// resend, whatever error the stream's last Recv returned. A forced resync is
// classified before the context is looked at, so it needs its own guard; its
// token is still dropped, as on any DataLoss.
func TestConsumeStream_AbandonedResendShutdown(t *testing.T) {
	ends := map[string]error{
		"forced resync": status.Error(codes.DataLoss, "watch stream overflowed"),
		"eof":           io.EOF,
		"unavailable":   status.Error(codes.Unavailable, "connection reset"),
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			filter := []string{"default/a", "default/b"}
			r, logs := newLoggingRegistry(t, nil)
			r.SetServiceFilter(filter)
			var reader *sdkmetric.ManualReader
			r.metrics, reader = newTestClientMetrics(t)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got, failure := r.consumeStream(ctx, &fakeWatchStream{
				events: []*registrarv1.WatchEndpointsResponse{
					epEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/a", "10.0.0.1", ""),
				},
				err: end,
			}, "", r.resumeFor(serviceSet(filter), ""))

			assert.Empty(t, got)
			assert.NoError(t, failure)
			assert.Nil(t, counterByReason(t, reader, abandonedMetric), "a shutdown is not counted")
			assert.NotContains(t, logs.String(), abandonedLog)
		})
	}
}
