package cniconflist

import (
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// unchainedNotReady is how long aether must have been observed MISSING from the
// node's active conflist before the agent reports NotReady.
//
// At least two full re-check intervals, deliberately. A competing writer's
// in-place `cp -f` walks the file through a truncated state that parses as
// garbage, and a single check landing in that window reads as unchained; one
// blip must never be able to fence a node. Three intervals also comfortably
// outlasts the fsnotify settle delay plus a repair.
//
// A constant, not a flag: proposal 031 retired knobs whose only correct value is
// the default, and the kill switch that matters (--cni-conflist-reassert=false)
// already disables this check along with the rest of the loop.
const unchainedNotReady = 3 * DefaultCheckInterval

// ReadyChecker returns a healthz.Checker that fails once this node has been
// unable to mesh new pods for unchainedNotReady.
//
// The point is not the agent's own health — the agent is perfectly fine — but
// the node's. A node whose conflist has lost the aether entry keeps accepting
// pods that come up outside the mesh, silently (#667). Reporting NotReady lets
// the CONTROLLER's node-taint guard (proposal 033) re-arm the taint through the
// signal it already consumes, without making the agent a node-taint writer —
// the alternative proposal 033 explicitly rejected and this does not reopen.
//
// Two states pass deliberately:
//
//   - unknown (the loop has not completed a check yet), so this can never delay
//     agent startup. The boot window is already covered by the taint gate, which
//     HOLDS an existing taint until chaining is positively observed.
//   - a nil ChainState (--cni-conflist-reassert=false), so the kill switch
//     disables this check exactly as it disables the taint condition.
//
// Recovery needs no intervention: the re-assert loop re-checks every 60s, and
// the moment the entry is back the check passes again.
func ReadyChecker(cs ChainState) healthz.Checker {
	return func(*http.Request) error {
		if cs == nil {
			return nil
		}
		s := cs.ChainStatus()
		if !s.Observed || s.Chained {
			return nil
		}
		if unchained := time.Since(s.Since); unchained < unchainedNotReady {
			return nil
		}
		return fmt.Errorf(
			"aether is not chained in the node's active CNI conflist (since %s); pods created on this node are not being meshed",
			s.Since.Format(time.RFC3339),
		)
	}
}
