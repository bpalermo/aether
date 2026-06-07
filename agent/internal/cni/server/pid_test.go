package server

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPidFromProcNetnsPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    int32
		wantErr bool
	}{
		{name: "valid", path: "/proc/1234/ns/net", want: 1234},
		{name: "valid single digit", path: "/proc/7/ns/net", want: 7},
		{name: "non-numeric pid", path: "/proc/self/ns/net", wantErr: true},
		{name: "too short", path: "/proc//ns/net", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pidFromProcNetnsPath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolvePIDFromNetns_MatchesOwnNetns verifies the inode-match logic against
// the test process's own network namespace: the resolved PID must share the same
// nsfs inode as /proc/self/ns/net.
func TestResolvePIDFromNetns_MatchesOwnNetns(t *testing.T) {
	self, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Skipf("cannot stat /proc/self/ns/net: %v", err)
	}
	selfSys := self.Sys().(*syscall.Stat_t)

	pid, err := resolvePIDFromNetns("/proc/self/ns/net")
	require.NoError(t, err)
	require.Positive(t, pid)

	got, err := os.Stat("/proc/self/ns/net")
	require.NoError(t, err)
	gotSys := got.Sys().(*syscall.Stat_t)
	assert.Equal(t, selfSys.Ino, gotSys.Ino, "resolved PID should own the same netns")
}

func TestResolvePIDFromNetns_MissingPath(t *testing.T) {
	_, err := resolvePIDFromNetns("/does/not/exist/ns/net")
	require.Error(t, err)
}

func TestResolvePIDWithRetry_SucceedsAfterRetries(t *testing.T) {
	attempts := 0
	pid, err := resolvePIDWithRetry(context.Background(), func() (int32, error) {
		attempts++
		if attempts < 3 {
			return 0, errors.New("not yet")
		}
		return 4242, nil
	}, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, int32(4242), pid)
	assert.Equal(t, 3, attempts)
}

func TestResolvePIDWithRetry_TimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := resolvePIDWithRetry(ctx, func() (int32, error) {
		return 0, errors.New("never resolves")
	}, time.Millisecond)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
