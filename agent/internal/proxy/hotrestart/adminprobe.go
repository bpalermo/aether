package hotrestart

import (
	"context"
	"encoding/json"
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
