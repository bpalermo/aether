package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// subprocessEnv makes this test binary behave as identity-ready itself, so the
// exit-code assertions exercise the real main() (including os.Exit(1)).
const subprocessEnv = "AETHER_IDENTITY_READY_SUBPROCESS_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(subprocessEnv); ok {
		os.Args = append([]string{"identity-ready"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const podID = "spiffe://" + spiretest.TrustDomain + "/ns/aether-test/sa/client"

// fast returns options tuned for a test: quick retries, frequent waiting logs.
func fast(socket string) options {
	return options{
		socket:   socket,
		logEvery: 50 * time.Millisecond,
		retry:    20 * time.Millisecond,
		attempt:  2 * time.Second,
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func logger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// TestWait_ReadyReleasesImmediately: SPIRE already holds the pod's entry — the
// gate returns the pod's SPIFFE ID on the first fetch.
func TestWait_ReadyReleasesImmediately(t *testing.T) {
	fake, sock := spiretest.Start(t, podID)
	fake.StartServing()

	var logs syncBuffer
	id, err := wait(context.Background(), fast(sock), logger(&logs))
	require.NoError(t, err)
	assert.Equal(t, podID, id)
	assert.Equal(t, int64(1), fake.Fetches(), "one fetch is enough when the SVID is there")
	assert.Zero(t, fake.BadHeaders(), "every call carries the workload.spiffe.io security header")
	assert.Contains(t, logs.String(), "identity ready")
	assert.Contains(t, logs.String(), podID)
}

// TestWait_HoldsUntilSVIDExists is the #1053 window: the SPIRE agent is up but
// has no entry for the pod yet. The gate keeps waiting — logging what it waits
// for — and releases the moment the SVID is issued.
func TestWait_HoldsUntilSVIDExists(t *testing.T) {
	fake, sock := spiretest.Start(t, podID)

	var logs syncBuffer
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := wait(context.Background(), fast(sock), logger(&logs))
		done <- result{id, err}
	}()

	// Not ready: the gate must still be holding after several retries.
	require.Eventually(t, func() bool { return fake.Fetches() >= 3 }, 5*time.Second, 10*time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("gate released before the SVID existed: id=%q err=%v", r.id, r.err)
	default:
	}
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "still waiting") }, 5*time.Second, 10*time.Millisecond,
		"the periodic waiting line names what the gate is waiting for")
	assert.Contains(t, logs.String(), "socket="+sock)

	fake.StartServing()
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, podID, r.id)
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not release after the SVID was issued")
	}
}

// TestWait_TimeoutFailsWithAClearMessage: with --timeout set and no SVID, the
// gate gives up with errTimedOut naming the socket and the last error.
func TestWait_TimeoutFailsWithAClearMessage(t *testing.T) {
	_, sock := spiretest.Start(t, podID) // never serves

	o := fast(sock)
	o.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := wait(context.Background(), o, logger(io.Discard))
	require.ErrorIs(t, err, errTimedOut)
	assert.Contains(t, err.Error(), sock)
	assert.Contains(t, err.Error(), "no identity issued", "the last Workload API error is surfaced")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// scriptedWorkloadAPI is a Workload API whose first `refuse` fetches answer
// "no identity issued" at once and whose later fetches stall. With failEarly
// set, a stalled fetch fails with DeadlineExceeded that long BEFORE the
// deadline it received (grpc-timeout) — deterministically reproducing the
// #1190 race, where gRPC reports an attempt capped by the overall --timeout as
// DeadlineExceeded while the outer context's own timer has not fired yet.
type scriptedWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	refuse    int64
	failEarly time.Duration
	fetches   atomic.Int64
}

func (s *scriptedWorkloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	ctx := stream.Context()
	if s.fetches.Add(1) <= s.refuse {
		return status.Error(codes.PermissionDenied, "no identity issued")
	}
	if deadline, ok := ctx.Deadline(); ok && s.failEarly > 0 {
		select {
		case <-time.After(time.Until(deadline) - s.failEarly):
			return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	<-ctx.Done()
	return status.FromContextError(ctx.Err()).Err()
}

// startScripted serves s on a temporary UDS and returns the socket path.
func startScripted(t *testing.T, s *scriptedWorkloadAPI) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wlapi") // short path: the AF_UNIX budget
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "workload.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return sock
}

// TestWait_TimeoutKeepsTheLastMeaningfulError is #1190: SPIRE answers "no
// identity issued", then the next attempt — capped below --attempt-timeout by
// the overall --timeout — fails with DeadlineExceeded before the outer
// context's own timer fires. That attempt was only cut short, so the reported
// last error must stay SPIRE's real answer, not "context deadline exceeded".
func TestWait_TimeoutKeepsTheLastMeaningfulError(t *testing.T) {
	sock := startScripted(t, &scriptedWorkloadAPI{refuse: 1, failEarly: 150 * time.Millisecond})

	o := fast(sock)
	o.attempt = 10 * time.Second // every attempt after the first is capped by --timeout
	o.timeout = 400 * time.Millisecond
	_, err := wait(context.Background(), o, logger(io.Discard))
	require.ErrorIs(t, err, errTimedOut)
	assert.Contains(t, err.Error(), "no identity issued", "an attempt cut short by --timeout must not mask SPIRE's answer")
	assert.NotContains(t, err.Error(), "DeadlineExceeded")
}

// TestWait_StallingSPIREReportsDeadlineExceeded is the inverse: SPIRE never
// answers within a full --attempt-timeout. That timeout IS the truth about
// SPIRE, so it is the error reported.
func TestWait_StallingSPIREReportsDeadlineExceeded(t *testing.T) {
	sock := startScripted(t, &scriptedWorkloadAPI{})

	o := fast(sock)
	o.attempt = 50 * time.Millisecond
	o.timeout = 300 * time.Millisecond
	_, err := wait(context.Background(), o, logger(io.Discard))
	require.ErrorIs(t, err, errTimedOut)
	assert.Contains(t, err.Error(), "DeadlineExceeded",
		"an attempt that had its full budget and still timed out is SPIRE not answering")
}

// TestWait_NoSPIREAgentKeepsWaiting: nothing listens on the socket (spire-agent
// down, CSI driver not mounted yet). Not fatal — the gate waits like any other
// "not yet", and a timeout still bounds it.
func TestWait_NoSPIREAgentKeepsWaiting(t *testing.T) {
	o := fast(spiretest.UnservedSocket(t))
	o.timeout = 300 * time.Millisecond
	_, err := wait(context.Background(), o, logger(io.Discard))
	require.ErrorIs(t, err, errTimedOut)
}

// TestWait_CancelIsNotATimeout: SIGTERM (pod deleted while held) is reported as
// an interruption, not as a timeout.
func TestWait_CancelIsNotATimeout(t *testing.T) {
	_, sock := spiretest.Start(t, podID)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := wait(ctx, fast(sock), logger(io.Discard))
	require.Error(t, err)
	assert.False(t, errors.Is(err, errTimedOut))
	assert.ErrorIs(t, err, context.Canceled)
}

// TestParse pins the defaults (no timeout: fail closed) and rejects nonsense.
func TestParse(t *testing.T) {
	o, err := parse(nil, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, defaultWorkloadSocket, o.socket)
	assert.Zero(t, o.timeout, "the default is to wait forever (fail closed)")

	for _, args := range [][]string{
		{"--spire-workload-socket="},
		{"--timeout=-1s"},
		{"--log-every=0"},
		{"--retry-interval=0"},
		{"--nope"},
	} {
		_, err := parse(args, io.Discard)
		assert.Error(t, err, "args %v", args)
	}
}

// runMain runs this test binary as identity-ready with args and returns its exit
// code and combined output.
func runMain(t *testing.T, args string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), subprocessEnv+"="+args)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	require.NoError(t, err)
	return 0, string(out)
}

// TestMain_ExitCodes drives the real main(): exit 0 once the SVID exists (the
// kubelet then starts the app containers), exit 1 with a clear message on
// --timeout.
func TestMain_ExitCodes(t *testing.T) {
	fake, sock := spiretest.Start(t, podID)
	fake.StartServing()
	code, out := runMain(t, "--spire-workload-socket="+sock)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, podID)

	code, out = runMain(t, "--spire-workload-socket="+spiretest.UnservedSocket(t)+" --timeout=300ms --retry-interval=20ms")
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "identity-ready: timed out waiting for the pod's SVID")
}
