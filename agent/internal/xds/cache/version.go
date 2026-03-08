package cache

import (
	"fmt"
	"time"

	"go.uber.org/atomic"
)

func generateSnapshotVersion(label string, counter *atomic.Uint64) string {
	timestamp := time.Now().UnixMilli()
	version := counter.Add(1)
	return fmt.Sprintf("%d.%d.%s", timestamp, version, label)
}
