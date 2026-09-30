package udscsi

import (
	"fmt"
	"strconv"
	"strings"
)

// maxSize bounds --size. The tmpfs holds one socket inode and whatever the app
// writes next to it; anything near a gibibyte is a typo, and tmpfs pages are
// charged to the writer's memory cgroup, so it would be a typo that eats RAM.
const maxSize = 1 << 30

// ParseSize reads a tmpfs size cap in the Kubernetes binary-quantity spelling
// the chart uses ("1Mi", "512Ki") or as plain bytes ("1048576"), and returns it
// in bytes. It is deliberately not resource.ParseQuantity: that would link
// k8s.io/apimachinery into a binary whose deps_test forbids it.
func ParseSize(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(s)
	mult := int64(1)
	for suffix, m := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSuffix(s, suffix), m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: want bytes or an integer with a Ki/Mi/Gi suffix", orig)
	}
	if n <= 0 || n > maxSize/mult {
		return 0, fmt.Errorf("size %q: must be > 0 and <= 1Gi", orig)
	}
	return n * mult, nil
}
