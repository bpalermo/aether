// Command udsecho is the UDS-serving test workload for the proposal 034 e2e
// harness (e2e/uds.sh): a tiny HTTP server that listens ONLY on a Unix domain
// socket inside its pod's emptyDir and never binds a TCP port.
//
// That "never binds TCP" property is what makes the harness's assertions
// meaningful: the mesh advertises the pod at pod_ip:18008 and delivers over the
// socket, so any 200 a client sees proves the node proxy dialed the pipe. If UDS
// delivery silently fell back to TCP loopback, nothing would be listening, the
// delegated-liveness probe would fail, and the endpoint would stay unpromoted.
//
// It answers 200 on every path (the delegated-liveness probe uses GET / with
// Host: localhost over the same socket) with a one-line body naming the socket
// and the pod, so the harness can tell which workload served a request.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// socketMode keeps the socket owner/group-only: the node proxy runs as root and
// is never blocked by the mode, while other containers in the pod are kept out.
// This is the mode docs/workload-requirements.md asks real workloads to use.
const socketMode = 0o660

// shutdownGrace bounds the graceful drain after SIGTERM; the socket file is
// unlinked afterwards so a restarting container can bind it again.
const shutdownGrace = 5 * time.Second

func main() {
	socket := flag.String("socket", "", "path of the Unix socket to serve on (required)")
	text := flag.String("text", "hello-from-uds", "body marker so the caller can tell which workload answered")
	flag.Parse()

	if *socket == "" {
		log.Fatal("udsecho: --socket is required")
	}
	if err := run(*socket, *text); err != nil {
		log.Fatalf("udsecho: %v", err)
	}
}

func run(socket, text string) error {
	// A container restart leaves the previous socket file behind in the emptyDir
	// (the volume outlives the container), and bind(2) fails on an existing path.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket %q: %w", socket, err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", socket, err)
	}
	// Explicit chmod: the mode net.Listen creates is masked by the process umask.
	if err := os.Chmod(socket, socketMode); err != nil {
		return fmt.Errorf("chmod %q: %w", socket, err)
	}

	host, _ := os.Hostname()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "%s pod=%s socket=%s path=%s\n", text, host, socket, r.URL.Path)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("udsecho: serving on unix://%s (pod %s)", socket, host)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("udsecho: got %s, draining", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = srv.Shutdown(ctx)
	// Serve's listener close unlinks the socket, but do it unconditionally: a
	// Shutdown that times out must not leave a file the next start cannot bind.
	if rmErr := os.Remove(socket); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		log.Printf("udsecho: could not remove %s: %v", socket, rmErr)
	}
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
