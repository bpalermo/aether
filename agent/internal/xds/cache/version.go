package cache

import (
	"fmt"
	"time"

	"go.uber.org/atomic"
)

// generateSnapshotVersion creates a snapshot version string by combining a
// Unix millisecond timestamp, an atomic counter increment, and a label.
// The format is "timestamp.counter.label" (e.g., "1704067200000.42.listener").
// This ensures version strings are unique and ordered.
func generateSnapshotVersion(label string, counter *atomic.Uint64) string {
	timestamp := time.Now().UnixMilli()
	version := counter.Add(1)
	return fmt.Sprintf("%d.%d.%s", timestamp, version, label)
}
