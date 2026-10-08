//go:build !linux

package hotrestart

import "errors"

// affinityCPUs has no answer off Linux. Envoy's own default there is the
// hardware threads alone, which is what an error here falls back to.
func affinityCPUs() (int, error) {
	return 0, errors.New("CPU affinity is read on linux only")
}
