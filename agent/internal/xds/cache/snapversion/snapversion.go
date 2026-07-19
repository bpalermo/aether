// Package snapversion generates xDS snapshot version strings for the agent cache.
//
// A version string is "timestamp.counter.label" (e.g. "1704067200000.42.snapshot"):
// a Unix millisecond timestamp, an atomic counter increment, and a resource-type
// label. The timestamp+counter pair keeps versions unique and monotonically
// ordered so go-control-plane never applies an older snapshot after a newer one.
package snapversion

import (
	"fmt"
	"time"

	"go.uber.org/atomic"
)

// Generate creates a snapshot version string by combining a Unix millisecond
// timestamp, an atomic counter increment, and a label. The format is
// "timestamp.counter.label" (e.g., "1704067200000.42.listener"). This ensures
// version strings are unique and ordered.
func Generate(label string, counter *atomic.Uint64) string {
	timestamp := time.Now().UnixMilli()
	version := counter.Add(1)
	return fmt.Sprintf("%d.%d.%s", timestamp, version, label)
}
