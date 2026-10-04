package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/registry"
	"google.golang.org/protobuf/proto"
)

// Write-behind queue for external-registry writes.
//
// Agent-facing RPCs (RegisterEndpoint / UnregisterEndpoint, including health
// promotions) apply to the snapshot and broadcast to watchers IMMEDIATELY —
// discovery must move at watch latency — while the external registry (Cloud
// Map) write is queued here and retried with backoff. Decided 2026-06-11:
// eventual consistency toward the external registry (and therefore between
// clusters) is acceptable; what is NOT acceptable is a serving pod staying
// invisible to the mesh for up to a reconciliation sweep because one external
// write failed (the rev-68 roll regression: rolls drove the recorded healthy
// fraction to zero while real capacity existed).
//
// Pending-shielding: until an op's intent is BOTH flushed to the external
// registry AND observed back in a sync listing, the sync loop must not
// "correct" the snapshot toward the external registry's stale view (that
// would re-create the partial-world-view bug from the other direction).
// Overlay implements that by patching the fetched state with pending intents
// before the sync's Diff/Replace.
//
// Derived backends (registry.DerivedEndpoints, the kubernetes backend) are the
// exception to "observed back": their writes are no-ops and a listing derives
// each endpoint from its Pod, so a listing never reflects the agent's version
// of an endpoint field for field. Waiting for it pinned the agent's version on
// the receiving replica for the pod's lifetime while every other replica served
// the Pod's (aether#1145). There the Pod is the single source of truth: an
// intent is released by the first listing that STARTED after it was received,
// and is overlaid only onto a listing that may predate it (a sync already in
// flight when the RPC landed, which would otherwise regress the snapshot-first
// apply until the next sync).
const (
	wbInitialBackoff = 1 * time.Second
	wbMaxBackoff     = 30 * time.Second
	// wbMaxAge bounds how long an op may stay unflushed before it is dropped
	// (loud log + metric). The agents' reconnect re-assertion and ghost sweep
	// repair dropped registrations; a permanently failing external registry
	// must not shield the snapshot from reconciliation forever.
	wbMaxAge = 5 * time.Minute
	// wbTick is the queue's scan interval; per-op backoff is computed on top.
	wbTick = 500 * time.Millisecond
)

type wbKind int

const (
	wbRegister wbKind = iota
	wbUnregister
)

type wbKey struct {
	service  string
	protocol registryv1.Service_Protocol
	ip       string
}

type wbOp struct {
	kind     wbKind
	endpoint *registryv1.ServiceEndpoint // register/upsert payload (latest wins)

	attempts    int
	nextAttempt time.Time
	enqueued    time.Time
	// flushed: the external write succeeded; the op is kept only to shield the
	// snapshot until a sync observes the intent in the fetched state.
	flushed bool
}

// WriteBehindQueue flushes snapshot-first registry mutations to the external
// registry with retries, and shields the sync loop from un-observed intents.
// It implements controller-runtime's Runnable (all replicas).
type WriteBehindQueue struct {
	registry registry.Registry
	log      *slog.Logger
	metrics  *Metrics
	// derived: the backend ignores writes and derives every endpoint at listing
	// time (registry.DerivedEndpoints); see Overlay's release rule.
	derived bool

	mu  sync.Mutex
	ops map[wbKey]*wbOp

	// kick wakes the flush loop as soon as an op is enqueued (issue #1103), so
	// a fresh intent is written within one registry round trip instead of at
	// the next wbTick. Buffered 1: a burst of enqueues coalesces into one
	// flush, and the ticker still drives retries and backoff.
	kick chan struct{}
}

// NewWriteBehindQueue creates the queue. metrics may be nil.
func NewWriteBehindQueue(reg registry.Registry, log *slog.Logger, metrics *Metrics) *WriteBehindQueue {
	derived := false
	if d, ok := reg.(registry.DerivedEndpoints); ok {
		derived = d.DerivesEndpoints()
	}
	return &WriteBehindQueue{
		registry: reg,
		derived:  derived,
		log:      commonlog.Named(log, "write-behind"),
		metrics:  metrics,
		ops:      make(map[wbKey]*wbOp),
		kick:     make(chan struct{}, 1),
	}
}

// NeedLeaderElection returns false: each replica owns the external writes for
// the RPCs it received (peer replicas converge through the external registry
// until peer-watch lands).
func (q *WriteBehindQueue) NeedLeaderElection() bool { return false }

// EnqueueRegister records a register/upsert intent. A newer op for the same
// (service, protocol, ip) supersedes any older one.
func (q *WriteBehindQueue) EnqueueRegister(service string, protocol registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) {
	q.enqueue(wbKey{service, protocol, ep.GetIp()}, &wbOp{kind: wbRegister, endpoint: ep})
}

// EnqueueUnregister records a removal intent, superseding any pending register.
func (q *WriteBehindQueue) EnqueueUnregister(service string, protocol registryv1.Service_Protocol, ip string) {
	q.enqueue(wbKey{service, protocol, ip}, &wbOp{kind: wbUnregister})
}

func (q *WriteBehindQueue) enqueue(key wbKey, op *wbOp) {
	now := time.Now()
	op.enqueued = now
	op.nextAttempt = now // first attempt on the next tick
	q.mu.Lock()
	q.ops[key] = op
	depth := len(q.ops)
	q.mu.Unlock()
	q.metrics.wbDepth(context.Background(), depth)
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Start runs the flush loop until ctx ends. Implements Runnable.
func (q *WriteBehindQueue) Start(ctx context.Context) error {
	q.log.InfoContext(ctx, "write-behind queue started")
	ticker := time.NewTicker(wbTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			q.flushDue(ctx)
		case <-q.kick:
			// A new intent. Peer replicas see an endpoint change only once it
			// is in the external registry (their etcd watch), so waiting for
			// the next tick added up to wbTick to every cross-replica drain
			// mark (#1103: 0.5-0.9 s vs 0.2 s same-replica on kind).
			q.flushDue(ctx)
		}
	}
}

// flushDue attempts every due, unflushed op once.
func (q *WriteBehindQueue) flushDue(ctx context.Context) {
	now := time.Now()
	due := q.collectDueOps(ctx, now)
	for key, op := range due {
		q.flushOp(ctx, key, op)
	}
}

// collectDueOps locks the queue, collects ops that are due (not flushed, not
// waiting, not expired), drops those that exceeded wbMaxAge, and returns the
// collected ops. The caller must not hold q.mu.
func (q *WriteBehindQueue) collectDueOps(ctx context.Context, now time.Time) map[wbKey]wbOp {
	q.mu.Lock()
	due := make(map[wbKey]wbOp)
	for key, op := range q.ops {
		if op.flushed || now.Before(op.nextAttempt) {
			continue
		}
		if now.Sub(op.enqueued) > wbMaxAge {
			q.log.ErrorContext(ctx, "write-behind op exceeded max age; dropping (agent re-assertion/sweep will repair)", "error", nil,
				"service", key.service, "ip", key.ip, "kind", int(op.kind), "attempts", op.attempts)
			q.metrics.wbDropped(ctx)
			delete(q.ops, key)
			continue
		}
		due[key] = *op
	}
	q.mu.Unlock()
	return due
}

// flushOp performs one registry write for the given op and updates the op's
// state (backoff or flushed) under the queue lock. Skips the update when a
// newer op has superseded this one mid-flight.
func (q *WriteBehindQueue) flushOp(ctx context.Context, key wbKey, op wbOp) {
	var err error
	switch op.kind {
	case wbRegister:
		err = q.registry.RegisterEndpoint(ctx, key.service, key.protocol, op.endpoint)
	case wbUnregister:
		err = q.registry.UnregisterEndpoint(ctx, key.service, key.ip)
	}

	q.mu.Lock()
	cur, ok := q.ops[key]
	// A newer op may have superseded this one mid-flight; never touch it.
	superseded := !ok || cur.kind != op.kind || (op.kind == wbRegister && !proto.Equal(cur.endpoint, op.endpoint))
	if !superseded {
		if err != nil {
			cur.attempts++
			backoff := wbInitialBackoff << min(cur.attempts, 5) // 2s,4s,...,32s≈cap
			if backoff > wbMaxBackoff {
				backoff = wbMaxBackoff
			}
			cur.nextAttempt = time.Now().Add(backoff)
		} else {
			cur.flushed = true // kept for shielding until Overlay observes it
		}
	}
	q.mu.Unlock()

	if err != nil {
		q.metrics.wbFlushFailed(ctx)
		q.log.DebugContext(ctx, "write-behind flush failed; will retry",
			"service", key.service, "ip", key.ip, "error", err.Error())
	}
}

// Overlay reconciles the queue against a freshly fetched external-registry
// state and patches that state with still-pending intents. Called by the sync
// loop BEFORE Diff/Replace, so neither the broadcast events nor the snapshot
// regress an intent the external registry has not materialized yet.
//
// listedAt is when the sync started listing: every intent received before it
// is in what the listing returned.
//
// Release rule: a flushed op whose intent the fetched state reflects
// (register: key present with an equal endpoint; unregister: key absent) is
// done and removed. Everything else is overlaid onto the state.
//
// On a derived backend (kubernetes) the rule is the listing's age instead: an
// op received before listedAt is released and the listing is taken as it is, so
// every replica serves the same Pod-derived endpoint (aether#1145). An op
// received after listedAt is overlaid as above.
//
// It returns how many intents it overlaid: nonzero means the state is no longer
// exactly the store's listing, which the snapshot version must say (#1193).
func (q *WriteBehindQueue) Overlay(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, listedAt time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	shielded := 0
	for key, op := range q.ops {
		if q.derived && op.enqueued.Before(listedAt) {
			delete(q.ops, key) // listed after the intent: the Pod decides
			continue
		}
		released, overlaid := overlayOp(state, key, op)
		if released {
			delete(q.ops, key)
		}
		if overlaid {
			shielded++
		}
	}
	if shielded > 0 {
		q.metrics.wbShielded(context.Background(), shielded)
		q.log.Debug("overlaid pending write-behind intents onto sync state", "count", shielded)
	}
	q.metrics.wbDepth(context.Background(), len(q.ops))
	return shielded
}

// overlayOp applies Overlay's observed-back rule to one op: released when the
// fetched state reflects the flushed intent, otherwise the intent is patched
// onto the state (overlaid reports whether that changed anything).
func overlayOp(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, key wbKey, op *wbOp) (released, overlaid bool) {
	fetched, present := findEndpoint(state, key)
	switch op.kind {
	case wbRegister:
		if op.flushed && present && proto.Equal(fetched, op.endpoint) {
			return true, false // intent observed; released
		}
		setEndpoint(state, key, op.endpoint)
		return false, true
	case wbUnregister:
		if op.flushed && !present {
			return true, false
		}
		if present {
			removeEndpoint(state, key)
			return false, true
		}
	}
	return false, false
}

func findEndpoint(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, key wbKey) (*registryv1.ServiceEndpoint, bool) {
	for _, ep := range state[key.service][key.protocol] {
		if ep.GetIp() == key.ip {
			return ep, true
		}
	}
	return nil, false
}

func setEndpoint(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, key wbKey, ep *registryv1.ServiceEndpoint) {
	if state[key.service] == nil {
		state[key.service] = make(map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint)
	}
	eps := state[key.service][key.protocol]
	for i, existing := range eps {
		if existing.GetIp() == key.ip {
			eps[i] = ep
			return
		}
	}
	state[key.service][key.protocol] = append(eps, ep)
}

func removeEndpoint(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, key wbKey) {
	eps := state[key.service][key.protocol]
	for i, existing := range eps {
		if existing.GetIp() == key.ip {
			state[key.service][key.protocol] = append(eps[:i], eps[i+1:]...)
			return
		}
	}
}

// Shielding reports whether an intent for (service, ip) is still pending or
// flushed-but-unobserved. Diagnostic/test helper.
func (q *WriteBehindQueue) Shielding(service, ip string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for key := range q.ops {
		if key.service == service && key.ip == ip {
			return true
		}
	}
	return false
}
