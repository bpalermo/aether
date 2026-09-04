package hotrestart

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Envoy-admin watchdog probes.
//
// Two endpoints answer the watchdog's question, and they answer different
// halves of it:
//
//   - /server_info returns the full server info document, including
//     command_line_options.restart_epoch — the only way to tell whether the
//     Envoy answering this node's shared admin port is OURS at OUR epoch.
//   - /ready returns a plain-text "<STATE>\n" (HTTP 200 iff LIVE) computed from
//     the same main-thread Utility::serverState(...) call, so it is equally
//     sensitive to the wedged-main-thread failure mode the watchdog exists to
//     detect — but it says nothing about which process answered.
//
// Steady state therefore rides /ready (no JSON, one pooled connection) and
// epoch identity rides /server_info on a fresh connection (see newAdminClients
// and adminProber).

// The liveness watchdog may ride the cheap pooled /ready only so long before it
// must re-confirm, ON A FRESH CONNECTION, that the Envoy answering this node's
// admin is still ours at our epoch. That budget is DERIVED from the effective
// config rather than hard-coded, because the value it must sit under does not
// live in this file: --parent-shutdown-time is 15s AS DEPLOYED
// (charts/aether/values.yaml, proxy.hotRestart.parentShutdownTime), while the CLI
// default in agent/internal/cmd/supervisor.go is 60s. A constant justified against
// the flag default was silently equal to the deployed value (#666).
//
// Why parent-shutdown-time is the bound: it is exactly how long a draining
// hot-restart parent lives, and that parent keeps answering LIVE at its OLD epoch
// over an already-accepted connection for the whole window (proposal 001 lessons 3
// and 6). Re-verification must land comfortably INSIDE that window so a cross-pod
// takeover is diagnosed while our parent is still around — not at or after its
// exit. Dividing by adminReverifyDivisor puts at least two full re-verifications
// inside the window at worst-case phase alignment. The same divisor is applied to
// AdminUnresponsiveDeadline, so the identity check can never lag the watchdog that
// consumes it.
//
// Floor: two watchdog ticks. Below that the two-tier prober degenerates into an
// authoritative /server_info every other tick and #646's saving is gone. Ceiling:
// 15s, the value this used to be hard-coded at — a longer blind window buys
// nothing measurable (at 5s the cheap path still covers 4 ticks in 5) and #673
// shows the supervisor's dominant cost is the readiness re-exec, not this probe.
//
// Effective: deployed 15s/30s -> 5s. CLI defaults 60s/30s -> 10s.
const (
	adminReverifyDivisor = 3
	adminReverifyFloor   = 2 * readyPollInterval // two watchdog ticks
	adminReverifyCeiling = 15 * time.Second
)

// adminReverifyIntervalFor derives the re-verify budget from the two durations
// that bound it. A non-positive parentShutdown means "unset, not bounding".
func adminReverifyIntervalFor(parentShutdown, adminUnresponsive time.Duration) time.Duration {
	budget := adminUnresponsive
	if parentShutdown > 0 && parentShutdown < budget {
		budget = parentShutdown
	}
	return min(max(budget/adminReverifyDivisor, adminReverifyFloor), adminReverifyCeiling)
}

// adminReverifyInterval is the derived budget for this supervisor's effective
// configuration.
func (s *Supervisor) adminReverifyInterval() time.Duration {
	return adminReverifyIntervalFor(s.cfg.ParentShutdownTime, s.adminUnresponsiveDeadline())
}

// checkAdminReverifyMargin reports the residual case the floor introduces: a
// parent-shutdown-time so short that even the minimum re-verify interval cannot
// sit comfortably inside it. Deliberately not fatal — a lost diagnostic margin
// must never cost the node its data plane.
func (s *Supervisor) checkAdminReverifyMargin() error {
	interval := s.adminReverifyInterval()
	budget := s.adminUnresponsiveDeadline()
	if pst := s.cfg.ParentShutdownTime; pst > 0 && pst < budget {
		budget = pst
	}
	if interval*adminReverifyDivisor <= budget {
		return nil
	}
	return fmt.Errorf(
		"derived admin re-verify interval %s (floor %s) does not fit %d times inside the %s budget "+
			"(parent-shutdown-time %s, admin-unresponsive-deadline %s); raise parent-shutdown-time to at least %s",
		interval, adminReverifyFloor, adminReverifyDivisor, budget,
		s.cfg.ParentShutdownTime, s.adminUnresponsiveDeadline(),
		adminReverifyFloor*adminReverifyDivisor,
	)
}

// logAdminReverifyBudget records the derived re-verify interval and its inputs
// at startup, and flags a lost margin as an ERROR without refusing to start.
func (s *Supervisor) logAdminReverifyBudget(ctx context.Context) {
	s.log.InfoContext(
		ctx, "derived admin re-verify interval",
		"reverifyInterval", s.adminReverifyInterval(),
		"parentShutdownTime", s.cfg.ParentShutdownTime,
		"adminUnresponsiveDeadline", s.adminUnresponsiveDeadline(),
		"divisor", adminReverifyDivisor,
	)
	if err := s.checkAdminReverifyMargin(); err != nil {
		s.log.ErrorContext(ctx, "admin re-verify interval is not comfortably inside parent-shutdown-time; "+
			"a cross-pod takeover may not be diagnosed while our hot-restart parent is alive",
			"error", err)
	}
}

const (
	adminReadyBodyLimit      = 64      // "PRE_INITIALIZING\n" and friends
	adminServerInfoBodyLimit = 1 << 20 // /server_info is a few KB; cap the unbounded doc
	adminDialTimeout         = 1 * time.Second
	adminIdleConnTimeout     = 30 * time.Second
	adminLiveState           = "LIVE"
	epochUnverified          = -1
)

// newAdminClients builds the two Envoy-admin HTTP clients. The difference
// between them is a correctness invariant, not tuning:
//
// The authoritative client NEVER reuses a connection. A draining hot-restart
// parent keeps answering LIVE at its OLD epoch over already-accepted
// connections for the whole --parent-shutdown-time-s window (Envoy flips
// live_ in InstanceImpl::shutdown(), not in drainListeners()). Every
// epoch-identity decision — initStartEpoch (proposal 001 lesson 2),
// handleDebounce, and above all handleShutdown (lesson 3: the old pod detects
// a mid-handoff takeover by its own admin no longer answering at its epoch) —
// must therefore dial fresh, byte-for-byte today's semantics. A pooled answer
// there would invert lesson 3 and SIGTERM the successor's hot-restart parent:
// the errno-111 node data-plane gap of 2026-06-11.
//
// The fast client is used ONLY by /ready in the verified steady state, where
// the question is just "is the admin still answering LIVE". Measured on
// talos-main: cx_total == rq_total == 1/s/node with cx_active 0 and
// destroy_remote 100% — every probe was paying for a fresh TCP connection
// because the JSON decoder left the body short of EOF, so Go never pooled it
// (#646). One pinned connection replaces all of them.
func newAdminClients() (authoritative, fast *http.Client) {
	dialer := &net.Dialer{Timeout: adminDialTimeout}
	authoritative = &http.Client{Transport: &http.Transport{
		DialContext:        dialer.DialContext,
		DisableKeepAlives:  true,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		Proxy:              nil,
	}}
	fast = &http.Client{Transport: &http.Transport{
		DialContext:         dialer.DialContext,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
		Proxy:               nil,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     adminIdleConnTimeout,
	}}
	return authoritative, fast
}

// adminProber is the liveness watchdog's two-tier probe: it answers each tick
// with (live, reachable), spending a full /server_info round trip only while
// the epoch is unverified, and riding the cheap pooled /ready once /server_info
// has confirmed that the Envoy on this node's admin is ours at our epoch.
//
// It is owned exclusively by the watchLiveness goroutine and therefore holds no
// lock. The Run-goroutine call sites (adminLiveAtEpoch) deliberately bypass it:
// they need the authoritative answer, every time.
//
// Known benign divergence: a cross-pod successor taking over while our child is
// still alive is invisible to /ready for up to the derived re-verify interval.
// The readiness outcome is identical either way (both paths HOLD readiness while
// admin is reachable and our child is tracked), and the heartbeat write is a
// no-op under writeState's downgrade guard; only the diagnostic log is delayed.
type adminProber struct {
	s             *Supervisor
	reverify      time.Duration // derived from the effective config; see adminReverifyIntervalFor
	verifiedEpoch int           // epochUnverified disables the fast path
	verifiedAt    time.Time     // when /server_info last confirmed it
}

func newAdminProber(s *Supervisor) *adminProber {
	return &adminProber{s: s, reverify: s.adminReverifyInterval(), verifiedEpoch: epochUnverified}
}

// probe answers one watchdog tick. The invariant it maintains: the pinned
// /ready connection never survives a re-verification boundary or an ambiguous
// tick, so no epoch-identity answer can ever come off it.
func (p *adminProber) probe(ctx context.Context, epoch int) (live, reachable bool) {
	if p.fastPathValid(epoch) {
		live, reachable = p.s.adminReady(ctx)
		if live {
			return true, true
		}
		// Anything but a plain LIVE is ambiguous: resolve it authoritatively
		// from the next tick, and drop the pinned connection with it.
		p.invalidate()
		return false, reachable
	}
	// Re-verification must never be answered by a connection pinned to our own
	// (possibly superseded, possibly draining) process.
	p.invalidate()
	live, reachable = p.s.adminServerInfo(ctx, epoch)
	if live {
		p.verifiedEpoch, p.verifiedAt = epoch, time.Now()
	}
	return live, reachable
}

// fastPathValid reports whether /ready may stand in for /server_info this tick:
// the epoch is unchanged since the confirmation, the confirmation is recent,
// and our child for it is still tracked.
func (p *adminProber) fastPathValid(epoch int) bool {
	return p.verifiedEpoch == epoch &&
		time.Since(p.verifiedAt) < p.reverify &&
		p.s.childTracked(epoch)
}

func (p *adminProber) invalidate() {
	p.verifiedEpoch = epochUnverified
	p.s.adminFast.CloseIdleConnections()
}

// adminReady probes /ready on the pooled fast client: liveness and reachability
// only, with no epoch identity (see newAdminClients). It is valid ONLY once
// adminServerInfo has confirmed the epoch, which is what adminProber enforces.
func (s *Supervisor) adminReady(ctx context.Context) (live, reachable bool) {
	ctx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.AdminAddress+"/ready", nil)
	if err != nil {
		s.metrics.adminProbed(probeEndpointReady, probeResultUnreachable)
		return false, false
	}
	resp, err := s.adminFast.Do(req)
	if err != nil {
		s.metrics.adminProbed(probeEndpointReady, probeResultUnreachable)
		return false, false
	}
	// Read the (tiny) body to EOF BEFORE closing: draining the response is what
	// makes the connection reusable, and reuse is the entire point here.
	body, err := io.ReadAll(io.LimitReader(resp.Body, adminReadyBodyLimit))
	_ = resp.Body.Close()
	if err != nil {
		s.metrics.adminProbed(probeEndpointReady, probeResultUnreachable)
		return false, false
	}
	live = resp.StatusCode == http.StatusOK && strings.HasPrefix(string(body), adminLiveState)
	s.metrics.adminProbed(probeEndpointReady, probeResult(live))
	return live, true
}

// adminServerInfo distinguishes "admin answered but is not LIVE at epoch"
// (reachable, the normal mid-handoff state) from "admin did not answer at all"
// (connect failure or timeout — a wedged main thread leaves the admin socket
// bound but never accepting, so requests time out). The admin watchdog keys off
// reachable.
//
// It always runs on the unpooled authoritative client: the answer is only
// meaningful if it came from the process listening on the admin port right now,
// not from a connection pinned to a superseded or draining one. See
// newAdminClients.
func (s *Supervisor) adminServerInfo(ctx context.Context, epoch int) (live, reachable bool) {
	ctx, cancel := context.WithTimeout(ctx, readyPollInterval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.AdminAddress+"/server_info", nil)
	if err != nil {
		s.metrics.adminProbed(probeEndpointServerInfo, probeResultUnreachable)
		return false, false
	}
	resp, err := s.adminAuthoritative.Do(req)
	if err != nil {
		s.metrics.adminProbed(probeEndpointServerInfo, probeResultUnreachable)
		return false, false
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, adminServerInfoBodyLimit))
	_ = resp.Body.Close()
	var info struct {
		State              string `json:"state"`
		CommandLineOptions struct {
			RestartEpoch int `json:"restart_epoch"`
		} `json:"command_line_options"`
	}
	// Unmarshal (rather than Decoder.Decode) rejects trailing content the
	// streaming decoder would ignore; Envoy emits exactly one protojson object,
	// and reading the body whole is what a truncated transient response (a bare
	// 503) already looked like: a decode failure, i.e. reachable-but-not-live.
	if readErr != nil || json.Unmarshal(body, &info) != nil {
		s.metrics.adminProbed(probeEndpointServerInfo, probeResultNotLive)
		return false, true
	}
	live = info.State == adminLiveState && info.CommandLineOptions.RestartEpoch == epoch
	s.metrics.adminProbed(probeEndpointServerInfo, probeResult(live))
	return live, true
}
