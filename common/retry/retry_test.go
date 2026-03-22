package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDo_Success(t *testing.T) {
	var calls atomic.Int32
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		IsRetryable: func(error) bool { return true },
	}, func(_ context.Context) error {
		calls.Add(1)
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

func TestDo_RetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		IsRetryable: func(error) bool { return true },
	}, func(_ context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("transient")
		}
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

func TestDo_AllAttemptsExhausted(t *testing.T) {
	var calls atomic.Int32
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		IsRetryable: func(error) bool { return true },
	}, func(_ context.Context) error {
		calls.Add(1)
		return errors.New("always fails")
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 3 attempts")
	assert.Equal(t, int32(3), calls.Load())
}

func TestDo_NonRetryableError(t *testing.T) {
	var calls atomic.Int32
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		IsRetryable: func(error) bool { return false },
	}, func(_ context.Context) error {
		calls.Add(1)
		return errors.New("permanent")
	})

	require.Error(t, err)
	assert.Equal(t, "permanent", err.Error())
	assert.Equal(t, int32(1), calls.Load())
}

func TestDo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Do(ctx, Config{
		MaxAttempts: 3,
		BaseBackoff: time.Second,
		IsRetryable: func(error) bool { return true },
	}, func(_ context.Context) error {
		return errors.New("fail")
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGRPCTransient(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"Unavailable", status.Error(codes.Unavailable, "unavailable"), true},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "timeout"), true},
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad"), false},
		{"Internal", status.Error(codes.Internal, "internal"), false},
		{"nil", nil, false},
		{"non-gRPC", errors.New("plain error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.transient, GRPCTransient(tt.err))
		})
	}
}
