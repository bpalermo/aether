package setup

import (
	"context"
	"time"
)

// SetupTimeout bounds telemetry provider construction. Nothing in the current
// provider wiring dials a collector eagerly (the OTLP gRPC clients are lazy),
// but resource detection and any future exporter option could block, and
// telemetry must never be able to spend the caller's startup budget: a binary
// whose health probes only start answering after initialization is killed by
// the kubelet long before it serves anything (issue #662).
const SetupTimeout = 5 * time.Second

// FlushTimeout bounds the final flush of a telemetry provider on shutdown.
const FlushTimeout = 5 * time.Second

// DetachedTimeout returns a context that keeps parent's values but neither
// observes its cancellation nor propagates cancellation back to it, bounded by
// d. Use it for telemetry work that must be immune to — and invisible to — the
// caller's lifecycle context.
func DetachedTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}

// BestEffortShutdown wraps a provider shutdown so the flush always runs on a
// detached, bounded context, and returns nil when fn is nil.
//
// On SIGTERM the caller's context is ALREADY cancelled by the signal handler, so
// passing it straight through made every final flush a guaranteed no-op that
// failed with "context canceled" — the last records of a terminating process,
// which are exactly the ones worth having, were always dropped (issue #662).
func BestEffortShutdown(fn func(context.Context) error) func(context.Context) error {
	if fn == nil {
		return nil
	}
	return func(ctx context.Context) error {
		flushCtx, cancel := DetachedTimeout(ctx, FlushTimeout)
		defer cancel()
		return fn(flushCtx)
	}
}
