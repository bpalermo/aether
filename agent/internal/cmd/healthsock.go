package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
)

// healthSocketShutdown bounds the health server's graceful stop.
const healthSocketShutdown = 5 * time.Second

// newHealthMux serves the agent's liveness and readiness the way the
// manager's TCP probe server does (/healthz, /readyz, each with ?verbose and
// /<check> subpaths), over the given readiness checks plus a liveness ping.
func newHealthMux(ready map[string]healthz.Checker) *http.ServeMux {
	mux := http.NewServeMux()
	live := &healthz.Handler{Checks: map[string]healthz.Checker{"ping": healthz.Ping}}
	readyz := &healthz.Handler{Checks: ready}
	mux.Handle("/healthz", http.StripPrefix("/healthz", live))
	mux.Handle("/healthz/", http.StripPrefix("/healthz", live))
	mux.Handle("/readyz", http.StripPrefix("/readyz", readyz))
	mux.Handle("/readyz/", http.StripPrefix("/readyz", readyz))
	return mux
}

// listenHealthSocket binds the health socket, replacing a stale one. The path
// is pod-local (an emptyDir), so the only previous owner is an earlier
// container of this same pod, which is gone.
func listenHealthSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the health socket dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing a stale health socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on the health socket: %w", err)
	}
	return ln, nil
}

// wireHealthSocket serves /healthz and /readyz on cfg.HealthSocketPath for the
// agent-ready exec probe (proposal 041, "hostNetwork ports"). The agent pod is
// hostNetwork, so its TCP probe port is a NODE port: a surge-rolled standby
// could neither declare it (the scheduler keeps a pod with a colliding hostPort
// Pending) nor bind it, and a port shared with SO_REUSEPORT would let the other
// agent answer the kubelet for this one. A socket in the pod's own emptyDir is
// answered by this process or by nobody.
//
// It serves from the moment the manager starts — before this agent owns the
// node, which is the point: a standby's readiness ("standby": first snapshot
// built) is what lets the roll proceed. A no-op without --health-socket.
func wireHealthSocket(m ctrlmanager.Manager, ready map[string]healthz.Checker) error {
	if cfg.HealthSocketPath == "" {
		return nil
	}
	ln, err := listenHealthSocket(cfg.HealthSocketPath)
	if err != nil {
		return err
	}
	timeout := healthSocketShutdown
	srv := &ctrlmanager.Server{
		Name:            "health socket",
		Server:          &http.Server{Handler: newHealthMux(ready), ReadHeaderTimeout: time.Second},
		Listener:        ln,
		ShutdownTimeout: &timeout,
	}
	if err := m.Add(srv); err != nil {
		_ = ln.Close()
		return fmt.Errorf("failed to add the health socket server: %w", err)
	}
	l.Info("serving liveness and readiness on the pod-local health socket", "socket", cfg.HealthSocketPath)
	return nil
}
