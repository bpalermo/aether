package spire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	commonlog "aethermesh.dev/common/log"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"go.opentelemetry.io/otel"
)

// ErrNoSVIDYet is what every SVIDSource method of a WaitingSource returns until
// the Workload API has issued the first SVID. It is a WAITING state, not a
// failure: callers that can degrade (the registrar watch stream, Envoy's SDS,
// the readiness gate inside its dwell) treat it as "not yet" rather than as an
// error to report or to die on.
var ErrNoSVIDYet = errors.New("no SPIRE SVID yet: still waiting for the Workload API to issue this workload's first SVID")

const (
	// DefaultWaitWarnAfter is how long the wait for the first SVID stays at INFO
	// before escalating to WARN. Two minutes comfortably covers a cold boot
	// (SPIRE server + agent attesting a whole fleet at once) while still making a
	// real outage loud. Also the readiness dwell — see NotReadyDwell.
	DefaultWaitWarnAfter = 2 * time.Minute

	// attemptTimeout bounds ONE attempt at creating the Workload API source, so a
	// SPIRE agent that accepts the connection and then never answers cannot park
	// the loop forever without logging.
	attemptTimeout = 10 * time.Second

	// Backoff between attempts. Same policy as the SPIRE bridge's stream
	// re-subscribe and the registrar watch loop: 1s doubling to 30s, jittered so
	// a whole fleet restarting together does not retry in lockstep.
	waitBackoffInitial = 1 * time.Second
	waitBackoffMax     = 30 * time.Second
	waitJitterFraction = 0.2
)

// WaitingSource is an SVIDSource that acquires the underlying Workload API
// source in the background, retrying forever, and NEVER fails its caller.
//
// It replaces the one-shot, fatal creation that made every binary's startup a
// hard dependency on SPIRE being up (issue #740): a 25s bounded wait that
// expired exited the process, so a SPIRE server that was still coming up during
// a cold boot crash-looped the whole fleet — 2-4 restarts per node — for a
// condition that resolves itself in under a minute.
//
// Nothing downstream needs the process to die: the SPIRE->SDS bridge dials
// lazily and retries forever, the xDS cache skips upstream-mTLS injection until
// SetNodeIdentity lands, and Envoy tolerates the absent secret (#715). So the
// process starts, answers /healthz and /readyz, programs everything that does
// not need identity, and folds identity in the moment it arrives.
//
// Start is a controller-runtime Runnable. Until the first SVID lands, every
// source method returns ErrNoSVIDYet; afterwards they delegate to the real
// source, which go-spiffe keeps rotating for the process's lifetime.
type WaitingSource struct {
	socketPath string
	log        *slog.Logger
	warnAfter  time.Duration
	metrics    *waitMetrics

	// Retry policy; the package constants in NewWaitingSource, shortened in tests.
	attemptTimeout time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	// startedAt is when the wait began (construction, i.e. process start), which
	// is what the WARN escalation and the readiness dwell are measured from.
	startedAt time.Time

	// updated carries the SVIDSource update signal. Buffered with capacity 1 and
	// written non-blocking, matching workloadapi.X509Source.Updated: a consumer
	// that is not selecting on it right now still sees exactly one pending wake.
	// Exactly one consumer gets each wake, so it is the SDS bridge's channel.
	updated chan struct{}

	// ready is CLOSED when the first SVID lands. A closed channel broadcasts, so
	// unlike updated it can have any number of waiters — which is what the
	// trust-domain reconcile uses, without racing the bridge for a wake.
	ready     chan struct{}
	readyOnce sync.Once

	mu  sync.RWMutex
	src *Source
}

// DefaultComponent is the component name a WaitingSource is namespaced under
// when the caller passes no WithComponent: the node agent, which is where the
// wait shipped first (#741) and whose metric names are already on dashboards.
const DefaultComponent = "agent"

// WaitOption customises a WaitingSource. It exists so the four binaries that
// wait for an SVID share one implementation and one set of log lines while
// still reporting under their own names.
type WaitOption func(*waitOptions)

type waitOptions struct {
	component string
}

// WithComponent names the binary doing the waiting ("agent", "controller",
// "registrar", "edge"). It selects the metric namespace —
// aether.<component>.spire.wait_seconds and friends — so a wait can be attributed
// to the workload that is stuck without the instruments being redefined per
// binary. An empty name keeps DefaultComponent.
func WithComponent(component string) WaitOption {
	return func(o *waitOptions) {
		if component != "" {
			o.component = component
		}
	}
}

// NewWaitingSource returns a WaitingSource for the Workload API at socketPath.
// warnAfter is how long the wait stays at INFO before escalating to WARN (and
// the readiness dwell); a non-positive value means DefaultWaitWarnAfter.
// Nothing is dialled until Start runs.
func NewWaitingSource(socketPath string, warnAfter time.Duration, log *slog.Logger, opts ...WaitOption) *WaitingSource {
	if warnAfter <= 0 {
		warnAfter = DefaultWaitWarnAfter
	}
	o := &waitOptions{component: DefaultComponent}
	for _, opt := range opts {
		opt(o)
	}
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled); a
	// registration failure only disables instrumentation, never the wait.
	metrics, err := newWaitMetrics(otel.Meter(waitMeterName), o.component)
	if err != nil {
		log.Error("failed to create SPIRE wait metrics; continuing without instrumentation", "error", err)
	}

	w := &WaitingSource{
		socketPath:     socketPath,
		log:            commonlog.Named(log, "spire-identity"),
		warnAfter:      warnAfter,
		metrics:        metrics,
		attemptTimeout: attemptTimeout,
		backoffInitial: waitBackoffInitial,
		backoffMax:     waitBackoffMax,
		startedAt:      time.Now(),
		updated:        make(chan struct{}, 1),
		ready:          make(chan struct{}),
	}
	metrics.observeReadiness(w.HasSVID)
	return w
}

// Start acquires the Workload API source, retrying with bounded backoff until it
// succeeds or ctx ends, then relays the source's rotation signal for the rest of
// the process's life. It implements controller-runtime's Runnable and returns
// nil on shutdown: a SPIRE outage must never take the manager down with it.
func (w *WaitingSource) Start(ctx context.Context) error {
	if !w.acquire(ctx) {
		return nil // shutting down before the first SVID arrived
	}
	w.relayUpdates(ctx)
	return nil
}

// NeedLeaderElection reports false: every replica needs its own identity.
func (w *WaitingSource) NeedLeaderElection() bool { return false }

// acquire loops until the source is created and serving an SVID. It reports
// whether it succeeded (false = ctx ended first).
func (w *WaitingSource) acquire(ctx context.Context) bool {
	backoff := w.backoffInitial
	for attempt := 1; ; attempt++ {
		if w.attempt(ctx, attempt) {
			return true
		}
		wait := jitter(backoff)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		backoff = nextBackoff(backoff, w.backoffMax)
	}
}

// nextBackoff doubles d, capped at maxBackoff.
func nextBackoff(d, maxBackoff time.Duration) time.Duration {
	return min(d*2, maxBackoff)
}

// attempt makes one bounded attempt at creating the source and reading its first
// SVID, reporting whether the source is now serving. It never returns an error:
// a failure is logged (INFO, escalating to WARN past warnAfter) and retried.
func (w *WaitingSource) attempt(ctx context.Context, attempt int) bool {
	attemptCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout)
	defer cancel()

	// The source's own first-SVID bound is disabled (0): attemptCtx is the bound.
	src, err := NewSourceWithTimeout(attemptCtx, w.socketPath, 0)
	if err != nil {
		w.logWaiting(ctx, attempt, err)
		return false
	}

	td, err := firstIdentity(src)
	if err != nil {
		// A source that was created but cannot serve a COMPLETE identity is worse
		// than none: drop it and build a fresh one. This is the only path that
		// re-creates a source, which is why aether.agent.spire.source_restarts
		// staying 0 is a meaningful signal.
		_ = src.Close()
		w.metrics.sourceRestarted(ctx)
		w.logWaiting(ctx, attempt, err)
		return false
	}

	w.mu.Lock()
	w.src = src
	w.mu.Unlock()

	waited := time.Since(w.startedAt)
	w.metrics.waited(ctx, waited)
	w.readyOnce.Do(func() { close(w.ready) })
	w.signalUpdated()
	w.log.InfoContext(ctx, "obtained this workload's SVID from the SPIRE Workload API",
		"socket", w.socketPath, "attempts", attempt, "elapsed", waited.Round(time.Millisecond), "trustDomain", td)
	return true
}

// firstIdentity reads BOTH halves of this workload's mesh identity out of a
// freshly created source — its own SVID and the X.509 bundle for the trust
// domain that SVID belongs to — and returns the trust-domain name.
//
// Both, not just the SVID, because an mTLS peer needs both and they are what
// readiness has to mean. On 2026-09-07 the agent announced recovery on the SVID
// alone (readyz spire-svid ok, svid_ready=1) while every mTLS client kept
// failing `x509svid: could not get X509 bundle` — the bundle half of the
// identity was what the peers were actually blocked on, and readiness said
// nothing about it. go-spiffe fills both from the same Workload API update, so
// in practice this costs one extra map lookup and closes the window where they
// could ever disagree (issue #740, finding 3).
func firstIdentity(src SVIDSource) (string, error) {
	svid, err := src.GetX509SVID()
	if err != nil {
		return "", fmt.Errorf("fetching workload SVID: %w", err)
	}
	td := svid.ID.TrustDomain()
	if _, err := src.GetX509BundleForTrustDomain(td); err != nil {
		return "", fmt.Errorf("fetching the X.509 bundle for trust domain %q: %w", td.Name(), err)
	}
	return td.Name(), nil
}

// logWaiting emits the one line per attempt that makes the wait visible, at INFO
// until warnAfter has elapsed and at WARN after it. The elapsed counter is what
// distinguishes "SPIRE is still coming up" from "SPIRE is not coming".
func (w *WaitingSource) logWaiting(ctx context.Context, attempt int, err error) {
	elapsed := time.Since(w.startedAt)
	level := slog.LevelInfo
	if elapsed >= w.warnAfter {
		level = slog.LevelWarn
	}
	w.log.Log(ctx, level, "waiting for the SPIRE Workload API to issue this workload's SVID",
		"socket", w.socketPath, "attempt", attempt, "elapsed", elapsed.Round(time.Millisecond),
		"warnAfter", w.warnAfter, "error", err)
}

// relayUpdates forwards the acquired source's rotation signal onto this source's
// own Updated channel until ctx ends, so consumers can hold one channel across
// the acquisition.
func (w *WaitingSource) relayUpdates(ctx context.Context) {
	w.mu.RLock()
	src := w.src
	w.mu.RUnlock()
	if src == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-src.Updated():
			w.signalUpdated()
		}
	}
}

// signalUpdated wakes one waiter without ever blocking the caller.
func (w *WaitingSource) signalUpdated() {
	select {
	case w.updated <- struct{}{}:
	default:
	}
}

// Updated implements SVIDSource. The channel is sent on when the first SVID
// arrives and on every rotation after it. Each send wakes exactly ONE receiver;
// waiters that just want the first SVID should use Ready.
func (w *WaitingSource) Updated() <-chan struct{} { return w.updated }

// Ready returns a channel closed once the first SVID has been obtained. Any
// number of goroutines may wait on it.
func (w *WaitingSource) Ready() <-chan struct{} { return w.ready }

// HasSVID reports whether this workload's identity is complete: the first SVID
// has arrived AND the bundle for its trust domain is servable (see
// firstIdentity — the source is only published once both are). It is what the
// readiness gate, the xDS hold and the registrar client's identity check read,
// so all three mean the same thing an mTLS handshake means.
func (w *WaitingSource) HasSVID() bool {
	if w == nil {
		return false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.src != nil
}

// GetX509SVID implements x509svid.Source, returning ErrNoSVIDYet until the first
// SVID has been issued.
func (w *WaitingSource) GetX509SVID() (*x509svid.SVID, error) {
	src, err := w.source()
	if err != nil {
		return nil, err
	}
	return src.GetX509SVID()
}

// GetX509BundleForTrustDomain implements x509bundle.Source, returning
// ErrNoSVIDYet until the first SVID has been issued.
func (w *WaitingSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	src, err := w.source()
	if err != nil {
		return nil, err
	}
	return src.GetX509BundleForTrustDomain(td)
}

// Close releases the underlying source. It is safe to call before the source has
// been acquired (and safe on a nil receiver, i.e. with SPIRE disabled).
func (w *WaitingSource) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	src := w.src
	w.src = nil
	w.mu.Unlock()
	if src == nil {
		return nil
	}
	return src.Close()
}

// source returns the acquired source or ErrNoSVIDYet.
func (w *WaitingSource) source() (*Source, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.src == nil {
		return nil, fmt.Errorf("%w (socket %s)", ErrNoSVIDYet, w.socketPath)
	}
	return w.src, nil
}

// NotReadyDwell is how long a workload may be waiting for its first SVID before
// the spire-svid readiness check starts failing.
//
// It is deliberately EQUAL to the warn threshold, and the value is the
// non-obvious decision in issue #740. The controller's node-taint guard
// (proposal 033) re-arms aether.io/agent-not-ready:NoSchedule on any agent that
// has been NotReady for longer than its 30s grace (controller/internal/nodetaint/
// guard.go), and spire-server does NOT tolerate that taint. A gate that failed
// from t=0 would therefore turn a 40s SPIRE hiccup into a fleet-wide taint that
// prevents spire-server itself from being rescheduled — the outage would fence
// out its own cure. The dwell keeps a routine cold boot invisible (every agent
// on the 2026-09-07 boot had its SVID inside 2m15s of SPIRE recovering) while a
// genuine outage still escalates to NotReady, which is what an operator wants:
// no new pods scheduled onto a node that cannot give them an identity.
const NotReadyDwell = DefaultWaitWarnAfter

// ReadyCheckName is the /readyz check name every binary registers its identity
// gate under, so `readyz?verbose` reads the same on an agent, the controller, the
// registrar and the edge — and one runbook line covers all four.
const ReadyCheckName = "spire-svid"

// ReadyChecker returns a readiness check (assignable to controller-runtime's
// healthz.Checker) that fails once this workload has been waiting for its first
// SVID for longer than the wait's warn threshold.
//
// A nil source (SPIRE disabled) always passes: with --spire-enabled=false there
// is no identity to wait for and the check must disappear entirely, exactly as
// the CNI chaining check disappears with its kill switch.
func ReadyChecker(w *WaitingSource) func(*http.Request) error {
	return func(*http.Request) error {
		if w == nil || w.HasSVID() {
			return nil
		}
		if waiting := time.Since(w.startedAt); waiting < w.warnAfter {
			return nil // inside the dwell: still a normal startup
		}
		return fmt.Errorf(
			"no SPIRE SVID after %s (socket %s); this workload cannot serve or verify mesh identity",
			time.Since(w.startedAt).Round(time.Second), w.socketPath,
		)
	}
}

// jitter returns d plus up to waitJitterFraction of random jitter, so a fleet
// restarting together does not retry in lockstep.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(float64(d)*waitJitterFraction*rand.Float64())
}
