package gamma

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"aethermesh.dev/agent/internal/gatewaystatus"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A status more than one writer wants.
//
// Every node's agent writes the one status of a route, every write is a route
// event, and every agent reconciles every route event. Writers that compute
// the same status settle at once (judged.go). Two kinds do not:
//
//   - an agent of the chart before this one, during a roll: it writes
//     Accepted=True always and ResolvedRefs from the ReferenceGrants alone,
//     and overwrites what this build says of a route it finds a fault with;
//   - two agents of this build that hold DIFFERENT rows of one service (a
//     registry update one of them has not received): each judges by its own
//     tables, and neither takes the other's verdict.
//
// Left alone, each pair rewrites the route as fast as the API server answers
// (measured: 45 writes a second on one route). So an agent that finds its
// own entry overwritten with something else does not write it back at once
// for ever. It writes it back once immediately, since most overwrites are a
// writer that was a moment behind; after that it waits, 5 s before the next
// rewrite and twice as long before each one after, up to 5 minutes, and asks
// to be reconciled when the wait is over.
//
// The wait is for ONE thing: writing the same status again over a writer that
// overwrote it. A status this agent has not written before is written at
// once, always. So the ledger is told what the agent wants on every
// reconcile, also when the status already says it and nothing is written
// (stands): a fault that clears and comes back is two changes of what is
// wanted, and neither waits. (It was told only when it wrote. A fault that
// cleared into the status another writer had left was not a write, the ledger
// kept the old wish, and when the fault came back the agent was held back as
// if it were answering that writer: True in the status while the table
// answered 500, for up to five minutes.) A route is the object, not its name:
// one deleted and created again under the same name starts from nothing.
//
// The bound, per route and per node, while a disagreement lasts: the first
// write, one rewrite at once, then rewrites no closer than 5 s, 10 s, 20 s,
// 40 s, 80 s, 160 s and from there 5 minutes apart. That is at most 5 writes
// in the first minute and at most one every 5 minutes from about ten minutes
// on. The agent whose turn comes last has the last word, and an agent of this
// build always gets another turn: once the other writer stops (the roll
// completes, the rows arrive) its next rewrite stands.

const (
	contestFirstWait = 5 * time.Second
	contestMaxWait   = 5 * time.Minute
	// contestQuiet: a status that stood this long was not contested any
	// more, and the next overwrite is counted from the start.
	contestQuiet = 2 * contestMaxWait
)

// contest is what this agent remembers of one route's status.
type contest struct {
	// wanted is the status this agent last wanted for the route (wanted.String).
	wanted string
	// written: this agent has written wanted; last is when it last did.
	written bool
	last    time.Time
	// rewrites counts the times it wrote wanted AGAIN, over another writer's.
	rewrites int
}

// contestLedger is the reconciler's memory of the routes whose status it
// writes. The zero value is ready.
type contestLedger struct {
	mu     sync.Mutex
	routes map[string]*contest
}

// holdBack returns how long this agent must still wait before it writes
// wanted over a status that differs from it, zero when it may write now.
func (l *contestLedger) holdBack(key, wanted string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.routes[key]
	if c == nil || c.wanted != wanted || !c.written || c.rewrites == 0 || now.Sub(c.last) >= contestQuiet {
		// Nothing written for this route yet, something else is wanted than
		// before, what was written has been overwritten for the first time,
		// or for the first time in a long while: now.
		return 0
	}
	wait := contestMaxWait
	if shift := c.rewrites - 1; shift < 6 {
		wait = min(contestFirstWait<<shift, contestMaxWait)
	}
	return max(c.last.Add(wait).Sub(now), 0)
}

// wrote records that this agent wrote wanted as the route's status at now.
func (l *contestLedger) wrote(key, wanted string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.routes == nil {
		l.routes = map[string]*contest{}
	}
	c := l.routes[key]
	if c == nil || c.wanted != wanted {
		l.routes[key] = &contest{wanted: wanted, written: true, last: now}
		return
	}
	if now.Sub(c.last) >= contestQuiet {
		// It stood for a long while: whoever overwrote it now is not the
		// writer of then.
		c.rewrites = 0
	} else if c.written {
		c.rewrites++
	}
	c.written, c.last = true, now
}

// stands records that the route's status IS what this agent wants, without
// its having written it now. When that is another status than the one
// remembered, what the agent wants has changed, and the count starts again.
func (l *contestLedger) stands(key, wanted string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.routes[key]; c != nil && c.wanted != wanted {
		l.routes[key] = &contest{wanted: wanted, written: true, last: now}
	}
}

// contestKey names one route OBJECT: kind, namespace and name, and the UID
// the API server gave it, so that a route deleted and created again under the
// same name is another route.
func contestKey(kind string, obj interface {
	GetNamespace() string
	GetName() string
	GetUID() types.UID
},
) string {
	return kind + "/" + obj.GetNamespace() + "/" + obj.GetName() + "/" + string(obj.GetUID())
}

// keep forgets every route that is not in seen: the routes that are gone.
func (l *contestLedger) keep(seen map[string]struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.routes {
		if _, ok := seen[key]; !ok {
			delete(l.routes, key)
		}
	}
}

// wanted is the status this agent computed for one route, as a string two
// reconciles of the same facts render alike: the route's generation and, per
// Service parentRef, the conditions.
type wanted struct{ b strings.Builder }

func newWanted(generation int64) *wanted {
	w := &wanted{}
	fmt.Fprintf(&w.b, "generation %d", generation)
	return w
}

func (w *wanted) add(p gatewayv1.ParentReference, conditions []gatewaystatus.Condition) {
	ns, port := "", int32(0)
	if p.Namespace != nil {
		ns = string(*p.Namespace)
	}
	if p.Port != nil {
		port = int32(*p.Port)
	}
	fmt.Fprintf(&w.b, "\n%s/%s:%d", ns, p.Name, port)
	for _, c := range conditions {
		fmt.Fprintf(&w.b, "|%s=%s/%s/%q", c.Type, c.Status, c.Reason, c.Message)
	}
}

func (w *wanted) String() string { return w.b.String() }

// now is the reconciler's clock.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
