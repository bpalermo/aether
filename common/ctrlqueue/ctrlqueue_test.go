package ctrlqueue

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const refreshLoop = "updateUnfinishedWorkLoop"

// refreshLoops counts live goroutines parked in a workqueue's 500 ms metrics
// refresh loop.
func refreshLoops(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), refreshLoop)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitForLoops polls until the refresh-loop count reaches want (goroutines are
// started asynchronously) or a short deadline passes, and returns the last count.
func waitForLoops(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	got := refreshLoops(t)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		got = refreshLoops(t)
	}
	return got
}

func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, time.Second)
}

// TestNewQueueStartsNoMetricsRefreshLoop is the issue #1131 guard: the queue
// every agent controller is built with must not start the 500 ms
// updateUnfinishedWorkLoop, while controller-runtime's default (named) queue —
// the control — does.
func TestNewQueueStartsNoMetricsRefreshLoop(t *testing.T) {
	before := refreshLoops(t)

	control := priorityqueue.New[reconcile.Request]("ctrlqueue-control")
	if got := waitForLoops(t, before+1); got != before+1 {
		t.Fatalf("control: a named default priority queue must start the refresh loop (got %d loops, want %d); the test no longer observes what it guards", got, before+1)
	}
	control.ShutDown()
	if got := waitForLoops(t, before); got != before {
		t.Fatalf("control queue's loop did not stop on ShutDown: %d loops, want %d", got, before)
	}

	q := NewQueue("gamma", rateLimiter())
	defer q.ShutDown()
	// Give a would-be loop the same chance to start as the control had.
	time.Sleep(100 * time.Millisecond)
	if got := refreshLoops(t); got != before {
		t.Fatalf("NewQueue started %d metrics refresh loop(s); want none", got-before)
	}
}

// TestNewQueueWorks checks the quiet queue is a functioning rate-limited queue.
func TestNewQueueWorks(t *testing.T) {
	q := NewQueue("gamma", rateLimiter())
	defer q.ShutDown()

	req := reconcile.Request{}
	req.Name, req.Namespace = "svc", "ns"
	q.Add(req)
	got, shutdown := q.Get()
	if shutdown || got != req {
		t.Fatalf("Get() = %v, %v; want %v, false", got, shutdown, req)
	}
	q.Done(got)

	q.AddRateLimited(req)
	got, _ = q.Get()
	if got != req {
		t.Fatalf("rate-limited Get() = %v; want %v", got, req)
	}
	q.Forget(got)
	q.Done(got)
}

// TestOptionsCarriesNewQueue checks Options wires NewQueue (the builders rely on
// it) and the provider type satisfies workqueue.MetricsProvider.
func TestOptionsCarriesNewQueue(t *testing.T) {
	if Options().NewQueue == nil {
		t.Fatal("Options().NewQueue is nil")
	}
	var _ workqueue.MetricsProvider = NoopMetricsProvider{}
}
