// Command agent-ready is the aether-agent pod's exec liveness and readiness
// probe (proposal 041).
//
// The agent pod is hostNetwork. A surge roll (agent.updateStrategy.surge) runs
// the new agent as a standby beside the old one on the same node, so no TCP
// port can serve the kubelet's probes: the surge pod's hostPort would collide
// (the scheduler keeps it Pending), the process could not bind it anyway, and a
// port shared with SO_REUSEPORT would let the OTHER agent answer for this one.
// So the agent serves /healthz and /readyz on a Unix socket in the pod's own
// /tmp emptyDir (--health-socket), and the kubelet execs this binary in the
// agent container to ask it.
//
// It is exec'd every probe period, so it must be TINY — the proxy-ready pattern
// (#673): package init() runs before main() and cannot be skipped by any argv
// check, so the only way not to pay for a dependency is not to link it. It is
// stdlib-only and does not even link net/http: it writes one HTTP/1.0 request
// by hand and reads the status line. //agent/cmd/agent-ready:deps_test fails
// the build if anything else is linked.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// defaultSocket must match agent/constants.DefaultAgentHealthSocketPath (pinned
// by main_test.go) and the --health-socket the chart passes the agent.
const defaultSocket = "/tmp/aether-agent-health.sock"

// maxBody bounds how much of a failing response is echoed for the kubelet's
// probe event (the verbose readyz body names the failing check).
const maxBody = 4096

// run parses args and returns nil iff the agent answered 200 on the path.
func run(args []string) error {
	fs := flag.NewFlagSet("agent-ready", flag.ContinueOnError)
	socket := fs.String("socket", defaultSocket, "The agent's health Unix socket (its --health-socket)")
	path := fs.String("path", "/readyz", "Endpoint to ask: /readyz or /healthz")
	timeout := fs.Duration("timeout", time.Second, "Bound on the whole exchange")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return probe(*socket, *path, *timeout)
}

func probe(socket, path string, timeout time.Duration) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("not ready: path %q must start with /", path)
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	// ?verbose makes a failing readyz name its failing check in the body.
	query := "?verbose"
	if strings.Contains(path, "?") {
		query = ""
	}
	if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.0\r\nHost: agent\r\n\r\n", path, query); err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("not ready: reading the status line: %w", err)
	}
	fields := strings.Fields(status)
	if len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") && fields[1] == "200" {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(r, maxBody))
	return fmt.Errorf("not ready: %s%s", strings.TrimSpace(status), bodyAfterHeaders(string(body)))
}

// bodyAfterHeaders returns the response body (after the blank line), prefixed
// for the error message, or "" when there is none.
func bodyAfterHeaders(rest string) string {
	_, body, found := strings.Cut(rest, "\r\n\r\n")
	if !found || strings.TrimSpace(body) == "" {
		return ""
	}
	return "\n" + strings.TrimSpace(body)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
