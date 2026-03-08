package cache

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

// TestGenerateSnapshotVersion verifies that the version string is non-empty,
// ends with the given label, and increments the counter on each call.
func TestGenerateSnapshotVersion(t *testing.T) {
	tests := []struct {
		name  string
		label string
	}{
		{name: "listener label", label: "listener"},
		{name: "cluster label", label: "cluster"},
		{name: "vhost label", label: "vhost"},
		{name: "empty label", label: ""},
		{name: "label with dots", label: "my.custom.label"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := atomic.NewUint64(0)
			got := generateSnapshotVersion(tt.label, counter)

			assert.NotEmpty(t, got)
			assert.Equal(t, uint64(1), counter.Load(), "counter should be incremented to 1 after one call")
			assert.True(t, strings.HasSuffix(got, "."+tt.label),
				"version string %q should end with .%s", got, tt.label)
		})
	}
}

// TestGenerateSnapshotVersion_CounterMonotonicallyIncreases verifies that successive
// calls produce strictly increasing counter values and distinct version strings.
func TestGenerateSnapshotVersion_CounterMonotonicallyIncreases(t *testing.T) {
	counter := atomic.NewUint64(0)
	label := "listener"

	v1 := generateSnapshotVersion(label, counter)
	v2 := generateSnapshotVersion(label, counter)
	v3 := generateSnapshotVersion(label, counter)

	assert.Equal(t, uint64(3), counter.Load())
	assert.NotEqual(t, v1, v2, "consecutive versions should differ")
	assert.NotEqual(t, v2, v3, "consecutive versions should differ")
}

// TestGenerateSnapshotVersion_ContainsTimestampCounterAndLabel verifies that the
// version string contains all three required parts: a non-zero timestamp, an
// incrementing counter, and the provided label.
func TestGenerateSnapshotVersion_ContainsTimestampCounterAndLabel(t *testing.T) {
	counter := atomic.NewUint64(9)
	label := "cluster"

	got := generateSnapshotVersion(label, counter)

	parts := strings.Split(got, ".")
	// Label may itself contain dots, but the prefix must be "timestamp.counter"
	require.GreaterOrEqual(t, len(parts), 3, "version should have at least 3 dot-separated parts: timestamp.counter.label")
	assert.NotEmpty(t, parts[0], "timestamp part should not be empty")
	assert.Equal(t, "10", parts[1], "counter part should be previous value + 1")
	assert.Equal(t, label, parts[len(parts)-1], "last part should be the label")
}
