package spire

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testTrustDomain = "example.org"

// fakeWorkloadAPI is a SPIRE Workload API server whose FetchX509SVID refuses to
// serve — exactly as a SPIRE agent that is up but cannot attest the workload yet
// — until startServing is called. That switch is the whole point: it reproduces
// the boot-time window in issue #740 without a container.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	svid *workload.X509SVID

	serving   atomic.Bool
	fetches   atomic.Int64
	badHeader atomic.Int64
}

func (f *fakeWorkloadAPI) startServing() { f.serving.Store(true) }

func (f *fakeWorkloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	f.fetches.Add(1)

	// The Workload API's security header. go-spiffe sets it on every call; a
	// real SPIRE agent rejects calls without it, so the fake asserts it too.
	md, _ := metadata.FromIncomingContext(stream.Context())
	if values := md.Get("workload.spiffe.io"); len(values) != 1 || values[0] != "true" {
		f.badHeader.Add(1)
		return status.Error(codes.InvalidArgument, "missing workload.spiffe.io header")
	}

	if !f.serving.Load() {
		return status.Error(codes.Unavailable, "no identity issued for this workload yet")
	}
	if err := stream.Send(&workload.X509SVIDResponse{Svids: []*workload.X509SVID{f.svid}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

// startFakeWorkloadAPI serves a fakeWorkloadAPI on a temporary UDS and returns
// it with the socket path. os.MkdirTemp("") keeps the path inside the ~108-byte
// AF_UNIX budget, which a Bazel sandbox path would blow (same reason as
// agent/internal/spire's fake SPIRE agent).
func startFakeWorkloadAPI(t *testing.T) (*fakeWorkloadAPI, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "wlapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "workload.sock")

	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	fake := &fakeWorkloadAPI{svid: newWorkloadSVID(t, "spiffe://"+testTrustDomain+"/ns/aether-system/sa/aether-agent")}
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return fake, sock
}

// newWorkloadSVID mints a CA plus a leaf carrying the SPIFFE ID as its URI SAN
// and marshals them the way the Workload API returns them (DER chain, PKCS#8
// key, DER bundle), so go-spiffe's own parser accepts them.
func newWorkloadSVID(t *testing.T, id string) *workload.X509SVID {
	t.Helper()

	sid := spiffeid.RequireFromString(id)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"SPIRE test CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{Organization: []string{"SPIRE test leaf"}},
		URIs:                  []*url.URL{sid.URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	require.NoError(t, err)

	return &workload.X509SVID{
		SpiffeId:    id,
		X509Svid:    leafDER,
		X509SvidKey: keyDER,
		Bundle:      caDER,
	}
}

// newTestWaitingSource builds a WaitingSource with the retry policy compressed
// so a test can watch several attempts go by, plus a recorder for its logs.
func newTestWaitingSource(t *testing.T, socket string, warnAfter time.Duration) (*WaitingSource, *lockedBuffer) {
	t.Helper()

	logs := &lockedBuffer{}
	w := NewWaitingSource(socket, warnAfter, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	w.attemptTimeout = 500 * time.Millisecond
	w.backoffInitial = 10 * time.Millisecond
	w.backoffMax = 50 * time.Millisecond
	return w, logs
}

// lockedBuffer is a concurrency-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records parses the JSON log lines emitted so far.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		out = append(out, rec)
	}
	return out
}

// waitingLines returns the per-attempt waiting records.
func (b *lockedBuffer) waitingLines(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, rec := range b.records(t) {
		if rec["msg"] == "waiting for the SPIRE Workload API to issue this workload's SVID" {
			out = append(out, rec)
		}
	}
	return out
}

// TestWaitingSourceIsNeverFatal is the regression test for #740: an unreachable
// Workload API must produce a source that keeps waiting — with an attempt/elapsed
// breadcrumb every time — and a Runnable that neither returns an error nor stops
// trying. Before the fix this whole path was one bounded call whose expiry exited
// the process.
func TestWaitingSourceIsNeverFatal(t *testing.T) {
	// A path nobody is serving: attempts fail, forever.
	dir, err := os.MkdirTemp("", "wlapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	w, logs := newTestWaitingSource(t, filepath.Join(dir, "absent.sock"), time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	require.Eventually(t, func() bool {
		return len(logs.waitingLines(t)) >= 3
	}, 30*time.Second, 10*time.Millisecond, "the wait must be announced once per attempt; logs:\n%s", logs.String())

	// The attempt counter increases and the elapsed counter never goes backwards:
	// that pair is what tells an operator "still coming up" from "not coming".
	lines := logs.waitingLines(t)
	var lastElapsed float64
	for i, rec := range lines {
		assert.Equal(t, float64(i+1), rec["attempt"], "attempts must be numbered consecutively")
		assert.Equal(t, "INFO", rec["level"], "a wait inside the warn threshold stays at INFO")
		elapsed, ok := rec["elapsed"].(float64) // slog renders a Duration as nanoseconds
		require.True(t, ok, "the wait line must carry an elapsed counter; got %v", rec)
		assert.GreaterOrEqual(t, elapsed, lastElapsed, "elapsed must not go backwards")
		lastElapsed = elapsed
	}
	assert.Positive(t, lastElapsed, "the elapsed counter must actually advance")

	assert.False(t, w.HasSVID(), "no SVID can have arrived")
	select {
	case err := <-done:
		t.Fatalf("Start returned instead of waiting: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a SPIRE outage must never fail the runnable")
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

// TestWaitingSourceEscalatesToWarn pins the escalation: past
// --spire-wait-warn-after the same line is logged at WARN, so a wait that has
// stopped being a normal boot is loud without ever being fatal.
func TestWaitingSourceEscalatesToWarn(t *testing.T) {
	dir, err := os.MkdirTemp("", "wlapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Warn immediately: every line must be WARN.
	w, logs := newTestWaitingSource(t, filepath.Join(dir, "absent.sock"), time.Nanosecond)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	require.Eventually(t, func() bool {
		return len(logs.waitingLines(t)) >= 2
	}, 30*time.Second, 10*time.Millisecond, "logs:\n%s", logs.String())

	for _, rec := range logs.waitingLines(t) {
		assert.Equal(t, "WARN", rec["level"], "past the warn threshold the wait must escalate")
		assert.Contains(t, rec, "error", "the line must carry why the attempt failed")
	}
}

// TestWaitingSourceBecomesReadyWhenSPIREArrives is the other half of the
// mechanism: the process that started without an identity picks one up the
// moment SPIRE starts serving, wakes its consumers, and records the wait once.
func TestWaitingSourceBecomesReadyWhenSPIREArrives(t *testing.T) {
	reader := installTestMeterProvider(t)

	fake, sock := startFakeWorkloadAPI(t)
	w, logs := newTestWaitingSource(t, sock, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Still refusing to attest: the source waits and serves ErrNoSVIDYet.
	require.Eventually(t, func() bool {
		return fake.fetches.Load() >= 1
	}, 30*time.Second, 10*time.Millisecond, "the source must be attempting; logs:\n%s", logs.String())
	require.False(t, w.HasSVID())
	_, err := w.GetX509SVID()
	require.ErrorIs(t, err, ErrNoSVIDYet)
	require.Equal(t, int64(0), fake.badHeader.Load(), "every call must carry the workload.spiffe.io header")
	require.Equal(t, int64(0), metricSum(t, reader, "aether.agent.spire.source_restarts"))
	require.Equal(t, int64(0), gaugeValue(t, reader, "aether.agent.spire.svid_ready"))

	fake.startServing()

	select {
	case <-w.Ready():
	case <-time.After(30 * time.Second):
		t.Fatalf("the source never became ready after SPIRE started serving; logs:\n%s", logs.String())
	}

	require.True(t, w.HasSVID())
	svid, err := w.GetX509SVID()
	require.NoError(t, err)
	assert.Equal(t, testTrustDomain, svid.ID.TrustDomain().Name())

	td, err := TrustDomainFromSource(w)
	require.NoError(t, err)
	assert.Equal(t, testTrustDomain, td)

	bundle, err := w.GetX509BundleForTrustDomain(spiffeid.RequireTrustDomainFromString(testTrustDomain))
	require.NoError(t, err)
	assert.NotEmpty(t, bundle.X509Authorities(), "the bundle must be served once the SVID lands")

	// The SDS bridge's wake: Updated fires on the first SVID, which is what
	// lets the node identity be served immediately rather than on the 30s tick.
	select {
	case <-w.Updated():
	case <-time.After(10 * time.Second):
		t.Fatal("Updated() did not fire when the first SVID arrived")
	}

	// The wait is recorded exactly once, and the readiness gauge flips.
	require.Equal(t, uint64(1), histogramCount(t, reader, "aether.agent.spire.wait_seconds"))
	require.Equal(t, int64(1), gaugeValue(t, reader, "aether.agent.spire.svid_ready"))
	require.Equal(t, int64(0), metricSum(t, reader, "aether.agent.spire.source_restarts"),
		"a source that comes up cleanly must never be re-created")

	ready := findRecord(logs.records(t), "obtained this workload's SVID from the SPIRE Workload API")
	require.NotNil(t, ready, "the arrival must be announced; logs:\n%s", logs.String())
	assert.Equal(t, testTrustDomain, ready["trustDomain"])
}

// TestWaitingSourceErrNoSVIDYet pins the pre-identity contract every consumer
// relies on: a typed, recognisable "not yet", not a generic failure.
func TestWaitingSourceErrNoSVIDYet(t *testing.T) {
	w := NewWaitingSource("/nonexistent/workload.sock", 0, slog.New(slog.DiscardHandler))

	_, err := w.GetX509SVID()
	require.ErrorIs(t, err, ErrNoSVIDYet)
	assert.Contains(t, err.Error(), "/nonexistent/workload.sock", "the error must name the socket")

	_, err = w.GetX509BundleForTrustDomain(spiffeid.RequireTrustDomainFromString(testTrustDomain))
	require.ErrorIs(t, err, ErrNoSVIDYet)

	assert.False(t, w.HasSVID())
	assert.NoError(t, w.Close(), "closing before acquisition is a no-op")
	assert.Equal(t, DefaultWaitWarnAfter, w.warnAfter, "a non-positive warn threshold means the default")
	assert.False(t, w.NeedLeaderElection(), "identity is per-replica, never leader-elected")
}

// TestWaitingSourceBackoffIsBounded pins the retry policy: doubling, capped, and
// jittered. Unbounded growth would leave a node minutes behind SPIRE's recovery;
// no growth would hammer a SPIRE agent that is already struggling.
func TestWaitingSourceBackoffIsBounded(t *testing.T) {
	got := waitBackoffInitial
	seen := []time.Duration{got}
	for range 10 {
		got = nextBackoff(got, waitBackoffMax)
		seen = append(seen, got)
	}
	assert.Equal(t, waitBackoffMax, got, "the backoff must saturate at the maximum")
	for i := 1; i < len(seen); i++ {
		assert.GreaterOrEqual(t, seen[i], seen[i-1], "the backoff must never shrink")
		assert.LessOrEqual(t, seen[i], waitBackoffMax, "the backoff must never exceed the maximum")
	}

	for range 100 {
		j := jitter(time.Second)
		assert.GreaterOrEqual(t, j, time.Second)
		assert.Less(t, j, time.Duration(float64(time.Second)*(1+waitJitterFraction)))
	}
}

// TestReadyChecker is the readiness table, dwell included.
func TestReadyChecker(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)

	ready := NewWaitingSource("/nonexistent/workload.sock", time.Minute, slog.New(slog.DiscardHandler))
	ready.src = &Source{} // stand-in: only its presence is read

	waitingInsideDwell := NewWaitingSource("/nonexistent/workload.sock", time.Minute, slog.New(slog.DiscardHandler))

	waitingPastDwell := NewWaitingSource("/nonexistent/workload.sock", time.Minute, slog.New(slog.DiscardHandler))
	waitingPastDwell.startedAt = time.Now().Add(-2 * time.Minute)

	tests := []struct {
		name    string
		src     *WaitingSource
		wantErr bool
	}{
		{
			// The kill switch. --spire-enabled=false means there is no identity
			// to wait for, so the check must disappear exactly as the CNI
			// chaining check does with its own kill switch.
			name: "nil source (SPIRE disabled) always passes",
			src:  nil,
		},
		{
			name: "holding an SVID passes",
			src:  ready,
		},
		{
			// The decision this dwell exists for: a node must not go NotReady
			// during the boot window, because the controller's taint guard
			// re-arms on 30s of NotReady and spire-server does not tolerate
			// that taint.
			name: "waiting inside the dwell passes",
			src:  waitingInsideDwell,
		},
		{
			name:    "waiting past the dwell fails",
			src:     waitingPastDwell,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ReadyChecker(tc.src)(req)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "no SPIRE SVID after")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestNotReadyDwellOutlastsTaintGuardGrace pins the cross-component relationship
// the dwell exists for. The controller's node-taint guard re-arms
// aether.io/agent-not-ready:NoSchedule after this many seconds of an agent being
// NotReady (grace, controller/internal/nodetaint/guard.go), and spire-server does
// not tolerate that taint — so if this dwell were ever shortened below it, a
// SPIRE outage would taint every node and block spire-server's own rescheduling.
func TestNotReadyDwellOutlastsTaintGuardGrace(t *testing.T) {
	const taintGuardGrace = 30 * time.Second
	assert.Greater(t, NotReadyDwell, taintGuardGrace,
		"the spire-svid dwell must outlast the node-taint guard's grace")
	assert.Equal(t, DefaultWaitWarnAfter, NotReadyDwell,
		"the dwell and the WARN threshold are deliberately the same instant")
}

// TestSourcesImplementSVIDSource keeps both sources interchangeable: the
// widened helpers must keep accepting the plain Workload API source that the
// controller, registrar and edge still create synchronously.
func TestSourcesImplementSVIDSource(t *testing.T) {
	var _ SVIDSource = (*WaitingSource)(nil)
	var _ SVIDSource = (*Source)(nil)
}

// installTestMeterProvider points the global meter provider at a manual reader
// for the duration of the test, so instruments registered by NewWaitingSource
// are collectable.
func installTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

// collect gathers the metric with the given name, or nil.
func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Aggregation {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	return nil
}

// metricSum returns a counter's total, 0 when it was never recorded.
func metricSum(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()

	sum, ok := collect(t, reader, name).(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

// gaugeValue returns an observable gauge's latest value.
func gaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()

	gauge, ok := collect(t, reader, name).(metricdata.Gauge[int64])
	require.True(t, ok, "metric %s is not an int64 gauge", name)
	require.NotEmpty(t, gauge.DataPoints)
	return gauge.DataPoints[len(gauge.DataPoints)-1].Value
}

// histogramCount returns how many observations a histogram holds.
func histogramCount(t *testing.T, reader *sdkmetric.ManualReader, name string) uint64 {
	t.Helper()

	hist, ok := collect(t, reader, name).(metricdata.Histogram[float64])
	require.True(t, ok, "metric %s is not a float64 histogram", name)
	var count uint64
	for _, dp := range hist.DataPoints {
		count += dp.Count
	}
	return count
}

// findRecord returns the first record whose msg matches.
func findRecord(records []map[string]any, msg string) map[string]any {
	for _, rec := range records {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}
