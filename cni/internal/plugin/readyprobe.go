package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/bpalermo/aether/common/constants"
	"golang.org/x/sys/unix"
)

// In-netns data-plane readiness probe.
//
// The agent's ACK wait only proves Envoy *accepted* the pod's listener config;
// this probe proves the data plane works: a full HTTP round trip against the
// outbound capture listener from inside the pod's netns, answered by the
// proxy's non-pass-through health_check filter on worker threads. A bare TCP
// connect is not enough — the kernel completes handshakes into the accept
// queue while the socket is bound but not yet accepted by workers. A 503 means
// the answering Envoy epoch is draining (hot restart in flight); retry until a
// serving epoch answers 200. Best-effort: timeouts are logged, never fail the
// CNI operation.

const (
	// readyProbeInterval is the delay between probe attempts.
	readyProbeInterval = 100 * time.Millisecond
	// readyProbeRequestTimeout bounds one probe round trip.
	readyProbeRequestTimeout = time.Second
	// readyProbeAddTimeout bounds the CNI ADD wait for the listener to serve.
	readyProbeAddTimeout = 5 * time.Second
	// readyProbeDelTimeout bounds the CNI DEL wait for the listener to be gone.
	readyProbeDelTimeout = 2 * time.Second
)

// readinessProber probes the proxy readiness endpoint inside one pod netns.
type readinessProber struct {
	client *http.Client
	url    string
}

// newReadinessProber builds a prober whose connections are dialed inside the
// network namespace at netnsPath.
func newReadinessProber(netnsPath string) *readinessProber {
	return &readinessProber{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: netnsDialContext(netnsPath),
				// Every attempt must re-dial: a kept-alive connection would
				// keep answering after the listener stops accepting.
				DisableKeepAlives: true,
			},
			Timeout: readyProbeRequestTimeout,
		},
		url: fmt.Sprintf("http://127.0.0.1:%d%s", constants.ProxyOutboundPort, constants.ProxyReadinessPath),
	}
}

// waitServing polls until the readiness endpoint answers 200 or ctx ends.
func (p *readinessProber) waitServing(ctx context.Context) error {
	ticker := time.NewTicker(readyProbeInterval)
	defer ticker.Stop()
	for {
		if code, err := p.probe(ctx); err == nil && code == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for proxy readiness at %s", p.url)
		case <-ticker.C:
		}
	}
}

// waitGone polls until dialing the listener fails with connection-refused (the
// proxy closed the listening socket) — or the netns itself is gone — or ctx
// ends. A 200/503 answer means an Envoy still holds the socket: keep waiting.
func (p *readinessProber) waitGone(ctx context.Context) error {
	ticker := time.NewTicker(readyProbeInterval)
	defer ticker.Stop()
	for {
		_, err := p.probe(ctx)
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for proxy listener removal at %s", p.url)
		case <-ticker.C:
		}
	}
}

// probe performs one GET against the readiness endpoint.
func (p *readinessProber) probe(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// netnsDialContext returns a DialContext that creates the connection's socket
// inside the network namespace at netnsPath: it locks the goroutine to its OS
// thread, setns()es into the target netns, dials (the socket inherits the
// thread's netns at creation), and restores the original netns. If the restore
// fails, the thread is left locked so the Go runtime destroys it rather than
// scheduling other goroutines onto a thread stuck in the pod's netns.
func netnsDialContext(netnsPath string) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := os.Open(netnsPath)
		if err != nil {
			return nil, fmt.Errorf("opening netns %s: %w", netnsPath, err)
		}
		defer func() { _ = target.Close() }()

		runtime.LockOSThread()
		origin, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
		if err != nil {
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("opening current netns: %w", err)
		}
		defer func() { _ = origin.Close() }()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("entering netns %s: %w", netnsPath, err)
		}

		conn, dialErr := dialer.DialContext(ctx, network, address)

		if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
			// Thread poisoned (stuck in the pod netns): keep it locked and die
			// with the goroutine.
			if conn != nil {
				_ = conn.Close()
			}
			return nil, fmt.Errorf("restoring host netns: %w", err)
		}
		runtime.UnlockOSThread()
		return conn, dialErr
	}
}
