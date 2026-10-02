package hotrestart

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Envoy-admin identity (issue #1127).
//
// The node proxy pods are hostNetwork, so 127.0.0.1:9901 is ONE address for the
// whole node: during a DaemonSet roll the old pod's supervisor, the surge
// successor's supervisor, and whichever Envoy currently holds the port all meet
// on it. Hot restart depends on that — the old pod detects a cross-pod takeover
// by the shared admin answering at a newer epoch — but it means "the Envoy that
// answers our admin address" is not "our Envoy". On 2026-10-01 the old pod's
// supervisor reached its no-successor fallback after its own Envoy had already
// closed its admin (a successor's hot-restart child asks the parent to shut its
// admin down, then crashed, #1126), and the new pod's supervisor had started a
// fresh epoch-0 Envoy that bound the port. POST /drain_listeners?graceful went
// to THAT Envoy, which then stopped adding listeners for every pod created on
// the node until the next handoff replaced it.
//
// The restart epoch cannot tell the two apart (a fresh Envoy of another pod is
// also epoch 0), and neither can a PID (each pod has its own PID namespace; the
// incident's Envoys were pid 16 and 17 in different pods). So every supervisor
// hands its own Envoy a per-supervisor nonce on a carrier /server_info echoes
// verbatim and that changes nothing else about the proxy:
// --admin-address-path. Its only effect is that Envoy writes its admin address
// into that file (a failure to write is logged, never fatal); the path is
// reported back as command_line_options.admin_address_path. The xDS node
// (--service-node, --service-cluster, --service-zone) is deliberately NOT used:
// the agent keys its configuration off it.
//
// Verifying the nonce and then sending the mutating request on a NEW connection
// would leave a window in which the port changes hands. Both therefore ride ONE
// TCP connection (see adminConn): Envoy serves a connection on the process that
// accepted it until the connection closes, so the request lands on exactly the
// process whose identity was just read, or fails.

// adminIdentityPrefix names the per-supervisor --admin-address-path file.
const adminIdentityPrefix = "envoy-admin-address."

// newAdminIdentity returns the --admin-address-path this supervisor passes to
// every Envoy it forks. The nonce is random per supervisor process; the pod
// name, when known, rides along so a WARN about a foreign answer can say whose
// Envoy it was. The file lives next to the ready marker (a pod-local emptyDir
// in the chart; the root filesystem is read-only), so it is also writable.
func newAdminIdentity(cfg Config) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; a time-derived fallback still
		// distinguishes two supervisors started at different instants.
		copy(b[:], fmt.Sprintf("%012d", time.Now().UnixNano()%1e12))
	}
	name := adminIdentityPrefix
	if cfg.PodName != "" {
		name += cfg.PodName + "."
	}
	name += hex.EncodeToString(b[:])
	return filepath.Join(adminIdentityDir(cfg), name)
}

func adminIdentityDir(cfg Config) string {
	if cfg.ReadyMarkerPath != "" {
		return filepath.Dir(cfg.ReadyMarkerPath)
	}
	return os.TempDir()
}

// removeStaleAdminIdentities deletes the address files earlier supervisor
// processes of this pod left in the (container-restart-surviving) emptyDir.
// Best-effort and only where the directory is the pod's own (ReadyMarkerPath
// set): the files are a few bytes each and carry nothing anyone reads.
func (s *Supervisor) removeStaleAdminIdentities() {
	if s.cfg.ReadyMarkerPath == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(adminIdentityDir(s.cfg), adminIdentityPrefix+"*"))
	for _, m := range matches {
		if m != s.adminIdentity {
			_ = os.Remove(m)
		}
	}
}

// adminServerInfoDoc is the subset of Envoy's /server_info the supervisor reads.
type adminServerInfoDoc struct {
	State              string `json:"state"`
	CommandLineOptions struct {
		RestartEpoch     int    `json:"restart_epoch"`
		AdminAddressPath string `json:"admin_address_path"`
	} `json:"command_line_options"`
}

// adminIdentityLabel renders whose Envoy an answer came from, for logs: the
// pod-name segment of its --admin-address-path when it has one.
func adminIdentityLabel(path string) string {
	if path == "" {
		return "unknown (no admin_address_path: not an aether supervisor's envoy, or one predating #1127)"
	}
	return filepath.Base(path)
}

// errAdminConnClosed reports that the admin closed the verified connection
// before the mutating request could be sent on it; sending it on a new one
// would defeat the verification.
var errAdminConnClosed = errors.New("envoy admin closed the verified connection")

// adminConn is one HTTP/1.1 connection to the Envoy admin, used for exactly
// one verify-then-mutate exchange. It deliberately bypasses http.Transport: a
// transport may silently dial a replacement connection, and a replacement
// connection may reach a different process.
type adminConn struct {
	addr   string
	conn   net.Conn
	br     *bufio.Reader
	closed bool
}

func dialAdmin(ctx context.Context, addr string) (*adminConn, error) {
	d := net.Dialer{Timeout: adminDialTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &adminConn{addr: addr, conn: c, br: bufio.NewReader(c)}, nil
}

func (a *adminConn) Close() error { return a.conn.Close() }

// do sends one request on the connection and reads its whole response, up to
// limit bytes of body. A body longer than limit leaves the stream out of step,
// so it is an error and the connection is unusable afterwards.
func (a *adminConn) do(ctx context.Context, method, path string, limit int64) (int, []byte, error) {
	if a.closed {
		return 0, nil, errAdminConnClosed
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = a.conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = a.conn.SetDeadline(time.Now()) })
	defer stop()

	req, err := http.NewRequestWithContext(ctx, method, "http://"+a.addr+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if err := req.Write(a.conn); err != nil {
		a.closed = true
		return 0, nil, err
	}
	resp, err := http.ReadResponse(a.br, req)
	if err != nil {
		a.closed = true
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		a.closed = true
		return 0, nil, err
	}
	if int64(len(body)) > limit {
		a.closed = true
		return 0, nil, fmt.Errorf("envoy admin %s response exceeds %d bytes", path, limit)
	}
	if resp.Close {
		a.closed = true
	}
	return resp.StatusCode, body, nil
}

// adminOwnership is the verdict of an identity check.
type adminOwnership string

const (
	adminOwn         adminOwnership = "own"
	adminForeign     adminOwnership = "foreign"
	adminUnreachable adminOwnership = "unreachable"
	// adminOwnGone: this supervisor tracks no Envoy at all, so whatever answers
	// the address cannot be ours and nothing is asked.
	adminOwnGone adminOwnership = "own_envoy_gone"
)

// ownsAdminAnswer reports whether a /server_info answer came from an Envoy this
// supervisor forked: it carries this supervisor's nonce AND names an epoch
// whose child is still tracked.
func (s *Supervisor) ownsAdminAnswer(info adminServerInfoDoc) bool {
	return info.CommandLineOptions.AdminAddressPath == s.adminIdentity &&
		s.childTracked(info.CommandLineOptions.RestartEpoch)
}

// ownAdminRequest sends a state-changing admin request ONLY to this
// supervisor's own Envoy: it reads /server_info and sends the request on the
// same connection, and only when the answer carries our identity. Anything
// else — no Envoy of ours left, the admin unreachable, a foreign Envoy
// answering (a cross-pod successor, or another pod's fresh Envoy) — is logged
// once at WARN and the request is NOT sent. The caller continues its path
// either way; only verdict adminOwn means the request was sent (err then says
// whether its response was read). request is the metric label for path.
func (s *Supervisor) ownAdminRequest(ctx context.Context, request, method, path string, limit int64) (
	verdict adminOwnership, status int, body []byte, err error,
) {
	defer func() { s.metrics.adminMutation(request, verdict) }()
	url := "http://" + s.cfg.AdminAddress + path
	if !s.anyChildTracked() {
		s.log.WarnContext(ctx, "not sending envoy admin request: this supervisor's envoy has exited, "+
			"so whatever answers the shared admin address is another pod's envoy",
			"request", method+" "+path, "url", url, "adminOwner", adminOwnGone)
		return adminOwnGone, 0, nil, nil
	}

	conn, err := dialAdmin(ctx, s.cfg.AdminAddress)
	if err != nil {
		s.log.WarnContext(ctx, "not sending envoy admin request: the admin did not answer the identity check",
			"request", method+" "+path, "url", url, "adminOwner", adminUnreachable, "error", err)
		return adminUnreachable, 0, nil, err
	}
	defer func() { _ = conn.Close() }()

	code, raw, err := conn.do(ctx, http.MethodGet, "/server_info", adminServerInfoBodyLimit)
	var info adminServerInfoDoc
	if err == nil && code == http.StatusOK {
		err = json.Unmarshal(raw, &info)
	} else if err == nil {
		err = fmt.Errorf("/server_info answered HTTP %d", code)
	}
	if err != nil {
		s.log.WarnContext(ctx, "not sending envoy admin request: the identity check failed",
			"request", method+" "+path, "url", url, "adminOwner", adminUnreachable, "error", err)
		return adminUnreachable, 0, nil, err
	}

	if !s.ownsAdminAnswer(info) {
		s.log.WarnContext(ctx, "not sending envoy admin request: the shared admin address is answered by "+
			"ANOTHER envoy, not this supervisor's (hostNetwork: every proxy pod on the node shares it)",
			"request", method+" "+path, "url", url, "adminOwner", adminForeign,
			"answeredBy", adminIdentityLabel(info.CommandLineOptions.AdminAddressPath),
			"answeredEpoch", info.CommandLineOptions.RestartEpoch,
			"answeredState", info.State,
			"ourIdentity", filepath.Base(s.adminIdentity),
			"ourEpoch", s.currentEpoch())
		return adminForeign, 0, nil, nil
	}

	status, body, err = conn.do(ctx, method, path, limit)
	if err != nil {
		// Our Envoy accepted the identity check and then dropped the
		// connection: never retry on a fresh one.
		return adminOwn, 0, nil, err
	}
	return adminOwn, status, body, nil
}
