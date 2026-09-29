// Command identity-ready is the egress-side identity gate (#1053): an init
// container the controller's pod-mutating webhook injects into every
// mesh-managed pod, which exits 0 only once SPIRE has issued the pod's X.509
// SVID. Until then the pod's app containers do not start, so the app cannot
// open a socket before the node proxy has a client certificate to present for
// it. It is the egress twin of the inbound-readiness promotion gate (an
// endpoint only turns HEALTHY after an mTLS handshake with the pod's own
// inbound listener).
//
// # Why an init container, and not the CNI ADD
//
// A pod that sends within ~8s of its CNI ADD used to have no SVID yet: the
// source proxy had no client certificate for it, so connects sat until
// connect_timeout and answered 503 UF (h2; 503 NC on QUIC before #1051). The
// decision on #1053 (2026-09-28) was NOT to block the CNI ADD: that would make
// SPIRE a hard dependency of sandbox creation, and the ADD races SPIRE's own
// pod-list attestation. Gating the app container instead keeps the sandbox and
// netns creation SPIRE-independent, lets SPIRE attest the pod normally, and
// makes a SPIRE outage visible as pods sitting in Init with a reason (this
// binary's log line) rather than as 503s.
//
// # Source of truth: the pod's own Workload API
//
// It asks the SPIRE agent's Workload API — mounted into THIS init container
// only, from the csi.spiffe.io CSI driver the mesh already requires — for the
// calling workload's X.509 SVID. SPIRE attests the caller by PID, so the answer
// is about this pod's own selectors (namespace, service account, pod UID,
// labels): the same registration entries the node agent's per-pod Broker API
// subscription resolves (proposal 036). "SPIRE issues this pod's SVID" is
// therefore observed directly, by the pod, with no new RPC on the node agent
// and no hostPath socket in the workload's pod spec (a hostPath volume would
// violate the Pod Security "baseline" profile; a CSI volume is allowed even
// under "restricted").
//
// The Workload API answers the moment the SPIRE agent holds an entry for the
// pod; the node agent's Broker API stream receives the same entry from the
// same SPIRE agent cache. The proxy fetches a pod's client certificate on
// demand at connect time (#842/#843), and a fetch that lands in the
// sub-second window before the node agent publishes the SVID waits for it
// rather than failing — so "the Workload API has the SVID" is the release
// point, with no settle delay.
//
// # Behaviour
//
// Fail closed: with no --timeout (the default) it waits forever, logging what
// it is waiting for every --log-every; the pod stays in Init. With --timeout it
// exits non-zero with a clear message once that much time has passed, and the
// kubelet restarts it with backoff (restartPolicy Always/OnFailure) or fails
// the pod (restartPolicy Never).
//
// It links gRPC, protobuf and go-spiffe's generated Workload API client — and
// nothing else heavy: no go-spiffe workloadapi/x509svid (go-jose), no
// controller-runtime, no client-go, no OTel. //agent/cmd/identity-ready:deps_test
// keeps it that way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// defaultWorkloadSocket is where the webhook mounts the csi.spiffe.io volume in
// the init container: the same path every aether component uses
// (spire.workloadSocketPath in the chart).
const defaultWorkloadSocket = "/run/secrets/workload-spiffe-uds/socket"

// options is the parsed command line.
type options struct {
	socket   string
	timeout  time.Duration
	logEvery time.Duration
	retry    time.Duration
	attempt  time.Duration
}

// errTimedOut is returned when --timeout elapses without an SVID.
var errTimedOut = errors.New("timed out waiting for the pod's SVID")

func parse(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("identity-ready", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.socket, "spire-workload-socket", defaultWorkloadSocket,
		"SPIRE Workload API UDS socket (the csi.spiffe.io mount)")
	fs.DurationVar(&o.timeout, "timeout", 0,
		"Give up and exit non-zero after this long without an SVID; 0 waits forever (fail closed: the pod stays in Init)")
	fs.DurationVar(&o.logEvery, "log-every", 10*time.Second,
		"How often to log what the gate is still waiting for")
	fs.DurationVar(&o.retry, "retry-interval", 500*time.Millisecond,
		"Pause between Workload API fetch attempts")
	fs.DurationVar(&o.attempt, "attempt-timeout", 15*time.Second,
		"Upper bound on one Workload API fetch attempt")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.socket == "" {
		return o, errors.New("--spire-workload-socket must not be empty")
	}
	if o.timeout < 0 || o.logEvery <= 0 || o.retry <= 0 || o.attempt <= 0 {
		return o, errors.New("--timeout must be >= 0; --log-every, --retry-interval and --attempt-timeout must be > 0")
	}
	return o, nil
}

// fetchOnce opens one FetchX509SVID stream and returns the SPIFFE ID of the
// first SVID it carries. SPIRE answers PermissionDenied ("no identity issued")
// while it has no entry for the caller; that, a missing socket, and an agent
// that is not up yet are all "not yet", reported as the error.
func fetchOnce(ctx context.Context, client workload.SpiffeWorkloadAPIClient) (string, error) {
	// The Workload API's security header: a SPIRE agent rejects calls without it.
	ctx = metadata.AppendToOutgoingContext(ctx, "workload.spiffe.io", "true")
	stream, err := client.FetchX509SVID(ctx, &workload.X509SVIDRequest{})
	if err != nil {
		return "", err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return "", err
		}
		for _, svid := range resp.GetSvids() {
			if svid.GetSpiffeId() != "" && len(svid.GetX509Svid()) > 0 {
				return svid.GetSpiffeId(), nil
			}
		}
		// An update with no usable SVID: keep reading the same stream.
	}
}

// wait blocks until the Workload API issues an SVID, the timeout elapses, or
// ctx is cancelled. It returns the SPIFFE ID it was issued.
func wait(ctx context.Context, o options, log *slog.Logger) (string, error) {
	start := time.Now()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	conn, err := grpc.NewClient("unix://"+o.socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("building the Workload API client for %s: %w", o.socket, err)
	}
	defer func() { _ = conn.Close() }()
	client := workload.NewSpiffeWorkloadAPIClient(conn)

	log.Info("holding the pod's containers until SPIRE issues its X.509 SVID (aether identity gate, #1053)",
		"socket", o.socket, "timeout", o.timeout.String())

	var (
		attempts int
		lastErr  error
		lastLog  = start
	)
	for {
		attempts++
		actx, cancel := context.WithTimeout(ctx, o.attempt)
		id, err := fetchOnce(actx, client)
		cancel()
		if err == nil {
			log.Info("identity ready: SPIRE issued this pod's SVID; releasing the pod's containers",
				"spiffe_id", id, "elapsed", time.Since(start).Round(time.Millisecond).String(), "attempts", attempts)
			return id, nil
		}
		// An attempt cut short by the overall timeout (or SIGTERM) says nothing
		// about SPIRE: keep the previous attempt's answer as the one to report.
		if lastErr == nil || ctx.Err() == nil {
			lastErr = err
		}

		if now := time.Now(); now.Sub(lastLog) >= o.logEvery {
			lastLog = now
			log.Warn("still waiting for SPIRE to issue this pod's SVID; the pod's containers stay held in Init "+
				"(check spire-agent on this node, the csi.spiffe.io driver, and that a registration entry matches this pod)",
				"socket", o.socket, "elapsed", time.Since(start).Round(time.Second).String(),
				"attempts", attempts, "last_error", lastErr.Error())
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("%w after %s (%d attempts, socket %s); last error: %v",
					errTimedOut, time.Since(start).Round(time.Second), attempts, o.socket, lastErr)
			}
			return "", fmt.Errorf("interrupted after %s without an SVID: %w", time.Since(start).Round(time.Second), ctx.Err())
		case <-time.After(o.retry):
		}
	}
}

// run is main without os.Exit, so tests drive it directly.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	o, err := parse(args, stderr)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	_, err = wait(ctx, o, log)
	return err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	err := run(ctx, os.Args[1:], os.Stderr)
	stop()
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "identity-ready:", err)
		}
		os.Exit(1)
	}
}
