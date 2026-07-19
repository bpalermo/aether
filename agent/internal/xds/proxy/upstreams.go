package proxy

import (
	"strings"

	xdsconst "github.com/bpalermo/aether/agent/internal/xds/xdsconst"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/serviceref"
)

// UpstreamsFromPod returns the upstream services the pod declares it consumes,
// parsed from the config.aether.io/upstreams annotation (comma-separated
// service names; surrounding whitespace is trimmed, empty items are dropped).
// Returns nil when the annotation is absent or empty — such a pod relies
// entirely on the on-demand cold path for its upstreams.
//
// Each value is normalized to a namespace-qualified "<ns>/<svc>" serviceref key
// (proposal 020 Part 1) so it matches the registry / dependency-set keys
// (cache.setPodDependencies keys the pod's own service via serviceref too) —
// otherwise a bare upstream name like "echo" never matches the registry's
// "<ns>/echo" key, so the demand-scoped cluster (and its cap_http route) never
// resolves:
//
//   - A bare name (no "/") is qualified with the POD's own namespace, so the
//     common same-namespace case keeps the ergonomic bare-name annotation.
//   - An already-qualified "<ns>/<svc>" value (containing "/") passes through
//     unchanged, for cross-namespace upstreams.
//
// Trimming, empty-item dropping, and dedup operate on the normalized keys so a
// bare name and its explicit "<pod-ns>/<name>" form collapse to one entry.
func UpstreamsFromPod(cniPod *cniv1.CNIPod) []string {
	raw, ok := cniPod.GetAnnotations()[xdsconst.AnnotationConfigUpstreams]
	if !ok || raw == "" {
		return nil
	}

	podNamespace := cniPod.GetNamespace()
	parts := strings.Split(raw, ",")
	upstreams := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		// Already namespace-qualified (cross-namespace upstream): pass through.
		// Otherwise qualify the bare name with the pod's own namespace.
		key := name
		if !strings.Contains(name, "/") {
			key = serviceref.New(podNamespace, name).Key()
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		upstreams = append(upstreams, key)
	}
	if len(upstreams) == 0 {
		return nil
	}
	return upstreams
}
