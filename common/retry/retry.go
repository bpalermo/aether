// Package retry provides a generic retry mechanism with exponential backoff.
// It is designed for operations that may fail transiently, such as gRPC calls
// or network requests, where retrying after a short delay can succeed.
package retry

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Config holds the retry parameters.
type Config struct {
	// MaxAttempts is the total number of attempts including the initial call.
	MaxAttempts int
	// BaseBackoff is the initial backoff duration. Each subsequent retry doubles it.
	BaseBackoff time.Duration
	// IsRetryable determines whether an error should be retried.
	IsRetryable func(error) bool
}

// Do executes fn, retrying on errors that IsRetryable returns true for.
// It uses exponential backoff between retries and respects context cancellation.
func Do(ctx context.Context, cfg Config, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := range cfg.MaxAttempts {
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}

		if !cfg.IsRetryable(lastErr) {
			return lastErr
		}

		if attempt < cfg.MaxAttempts-1 {
			backoff := cfg.BaseBackoff * time.Duration(1<<uint(attempt))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return fmt.Errorf("failed after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

// GRPCTransient returns true for gRPC errors that indicate a transient failure:
// Unavailable and DeadlineExceeded.
func GRPCTransient(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}
