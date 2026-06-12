package proxy

import (
	"strings"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
)

// UpstreamsFromPod returns the upstream services the pod declares it consumes,
// parsed from the config.aether.io/upstreams annotation (comma-separated
// service names; surrounding whitespace is trimmed, empty items are dropped).
// Returns nil when the annotation is absent or empty — such a pod relies
// entirely on the on-demand cold path for its upstreams.
func UpstreamsFromPod(cniPod *cniv1.CNIPod) []string {
	raw, ok := cniPod.GetAnnotations()[constants.AnnotationConfigUpstreams]
	if !ok || raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	upstreams := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		upstreams = append(upstreams, name)
	}
	if len(upstreams) == 0 {
		return nil
	}
	return upstreams
}
