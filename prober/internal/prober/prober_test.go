package prober

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

var traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-00$`)

func TestNotSampledTraceparent(t *testing.T) {
	tp := notSampledTraceparent()
	if !traceparentRe.MatchString(tp) {
		t.Fatalf("traceparent %q does not match the W3C not-sampled form", tp)
	}
	// The trace-id must be nonzero or Envoy rejects it and re-enables sampling.
	if strings.HasPrefix(tp, "00-00000000000000000000000000000000-") {
		t.Fatalf("traceparent has an all-zero trace-id: %q", tp)
	}
	// Two calls should differ (random ids), not a constant.
	if tp == notSampledTraceparent() {
		t.Fatalf("traceparent is not randomized: %q", tp)
	}
}

func TestClassifyErr(t *testing.T) {
	t.Run("connection refused -> connection_error", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		if got := classifyErr(context.Background(), err); got != resultConnectionError {
			t.Fatalf("got %q, want %q", got, resultConnectionError)
		}
	})
	t.Run("deadline -> timeout", func(t *testing.T) {
		if got := classifyErr(context.Background(), context.DeadlineExceeded); got != resultTimeout {
			t.Fatalf("got %q, want %q", got, resultTimeout)
		}
	})
	t.Run("wrapped DNS not-found -> dns_nxdomain", func(t *testing.T) {
		// Wrap the *net.DNSError so classifyErr must unwrap it via errors.As, mirroring
		// how the http transport surfaces a resolution failure inside an *url.Error.
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "bogus.aether.internal", IsNotFound: true})
		if got := classifyErr(context.Background(), err); got != resultDNSNXDomain {
			t.Fatalf("got %q, want %q", got, resultDNSNXDomain)
		}
	})
	t.Run("wrapped DNS timeout -> dns_timeout", func(t *testing.T) {
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "i/o timeout", Name: "slow.aether.internal", IsTimeout: true})
		if got := classifyErr(context.Background(), err); got != resultDNSTimeout {
			t.Fatalf("got %q, want %q", got, resultDNSTimeout)
		}
	})
	t.Run("wrapped generic DNS error -> dns_error", func(t *testing.T) {
		err := fmt.Errorf("dial: %w", &net.DNSError{Err: "server misbehaving", Name: "echo.aether.internal"})
		if got := classifyErr(context.Background(), err); got != resultDNSError {
			t.Fatalf("got %q, want %q", got, resultDNSError)
		}
	})
	t.Run("post-resolution dial error stays connection_error", func(t *testing.T) {
		// A dial failure with no *net.DNSError in the chain means the name resolved
		// and the connect failed — this must remain connection_error, not a dns_* class.
		err := fmt.Errorf("dial tcp 10.0.0.1:18081: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
		if got := classifyErr(context.Background(), err); got != resultConnectionError {
			t.Fatalf("got %q, want %q", got, resultConnectionError)
		}
	})
}

func TestNewMeshDNSTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MeshDNSTargets = []string{"echo.aether-test.aether.internal:18081", "echo.aether-test.aether.internal"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 1 liveness + 0 reachability + 2 mesh_dns.
	if len(p.targets) != 3 {
		t.Fatalf("want 3 targets (1 liveness + 2 mesh_dns), got %d", len(p.targets))
	}
	md := p.targets[1]
	if md.tier != tierMeshDNS {
		t.Fatalf("mesh_dns target tier = %q, want %q", md.tier, tierMeshDNS)
	}
	// The URL must be the REAL FQDN so the transport resolves it — not the fixed egress.
	if want := "http://echo.aether-test.aether.internal:18081/"; md.url != want {
		t.Fatalf("mesh_dns url = %q, want %q", md.url, want)
	}
	// authority MUST be empty: probe() must not override Host, or the name would never resolve.
	if md.authority != "" {
		t.Fatalf("mesh_dns authority = %q, want empty (no Host override)", md.authority)
	}
	// A target without an explicit port gets the default appended.
	if want := "http://echo.aether-test.aether.internal:18081/"; p.targets[2].url != want {
		t.Fatalf("mesh_dns default-port url = %q, want %q", p.targets[2].url, want)
	}
}

func TestNewTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReachabilityTargets = []string{"svc-1", "svc-2"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(p.targets) != 3 {
		t.Fatalf("want 3 targets (1 liveness + 2 reachability), got %d", len(p.targets))
	}
	if p.targets[0].tier != tierLiveness {
		t.Fatalf("first target tier = %q, want liveness", p.targets[0].tier)
	}
	if want := "http://127.0.0.1:18081/-/-/live"; p.targets[0].url != want {
		t.Fatalf("liveness url = %q, want %q", p.targets[0].url, want)
	}
	if want := "svc-1.aether.internal"; p.targets[1].authority != want {
		t.Fatalf("reachability authority = %q, want %q", p.targets[1].authority, want)
	}
}
