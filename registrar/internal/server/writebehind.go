package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
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

	mu  sync.Mutex
	ops map[wbKey]*wbOp
}

// NewWriteBehindQueue creates the queue. metrics may be nil.
func NewWriteBehindQueue(reg registry.Registry, log *slog.Logger, metrics *Metrics) *WriteBehindQueue {
	return &WriteBehindQueue{
		registry: reg,
		log:      commonlog.Named(log, "write-behind"),
		metrics:  metrics,
		ops:      make(map[wbKey]*wbOp),
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
		}
	}
}

// flushDue attempts every due, unflushed op once.
func (q *WriteBehindQueue) flushDue(ctx context.Context) {
	now := time.Now()

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

	for key, op := range due {
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
}

// Overlay reconciles the queue against a freshly fetched external-registry
// state and patches that state with still-pending intents. Called by the sync
// loop BEFORE Diff/Replace, so neither the broadcast events nor the snapshot
// regress an intent the external registry has not materialized yet.
//
// Release rule: a flushed op whose intent the fetched state reflects
// (register: key present with an equal endpoint; unregister: key absent) is
// done and removed. Everything else is overlaid onto the state.
func (q *WriteBehindQueue) Overlay(state map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint) {
	q.mu.Lock()
	defer q.mu.Unlock()

	shielded := 0
	for key, op := range q.ops {
		fetched, present := findEndpoint(state, key)

		switch op.kind {
		case wbRegister:
			if op.flushed && present && proto.Equal(fetched, op.endpoint) {
				delete(q.ops, key) // intent observed; released
				continue
			}
			setEndpoint(state, key, op.endpoint)
			shielded++
		case wbUnregister:
			if op.flushed && !present {
				delete(q.ops, key)
				continue
			}
			if present {
				removeEndpoint(state, key)
				shielded++
			}
		}
	}
	if shielded > 0 {
		q.metrics.wbShielded(context.Background(), shielded)
		q.log.Debug("overlaid pending write-behind intents onto sync state", "count", shielded)
	}
	q.metrics.wbDepth(context.Background(), len(q.ops))
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
