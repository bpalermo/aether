// Package ctrlqueue builds controller-runtime workqueues that keep no metrics,
// for components that do not export controller-runtime's workqueue_* series.
//
// Why (issue #1131): every controller-runtime priority queue built with a name
// starts an updateUnfinishedWorkLoop goroutine that wakes every 500 ms for the
// life of the process to refresh two Prometheus gauges. A node agent runs one
// queue per controller (gamma, l4route ×3, capture, endpointpolicy, node taint,
// ...), so that is ~10 timer wakeups a second on an otherwise idle process, and
// on a node where the Go runtime's threads land on deep-idle CPUs each one is
// expensive. Nothing scrapes the agent's workqueue_* series (the chart exposes
// :8080 but no scrape job, dashboard or alert reads it; the agent's own metrics go
// out over OTLP), so the loops are pure overhead.
//
// There is no manager-wide switch for this. The queue's metrics provider is not
// a manager option, and a no-op provider alone does not help: the priority queue
// starts the loop whenever its name is non-empty, whatever the provider. Only a
// queue with an empty name gets the no-metrics implementation and no loop, so
// NewQueue builds exactly that, and also passes NoopMetricsProvider so a future
// controller-runtime that keys the loop off the provider stays quiet too.
package ctrlqueue

import (
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NoopMetricsProvider is a workqueue.MetricsProvider whose metrics discard
// everything.
type NoopMetricsProvider struct{}

var _ workqueue.MetricsProvider = NoopMetricsProvider{}

type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Dec()            {}
func (noopMetric) Set(float64)     {}
func (noopMetric) Observe(float64) {}

// NewDepthMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewDepthMetric(string) workqueue.GaugeMetric { return noopMetric{} }

// NewAddsMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewAddsMetric(string) workqueue.CounterMetric { return noopMetric{} }

// NewLatencyMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewLatencyMetric(string) workqueue.HistogramMetric { return noopMetric{} }

// NewWorkDurationMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewWorkDurationMetric(string) workqueue.HistogramMetric {
	return noopMetric{}
}

// NewUnfinishedWorkSecondsMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewUnfinishedWorkSecondsMetric(string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}

// NewLongestRunningProcessorSecondsMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewLongestRunningProcessorSecondsMetric(string) workqueue.SettableGaugeMetric {
	return noopMetric{}
}

// NewRetriesMetric implements workqueue.MetricsProvider.
func (NoopMetricsProvider) NewRetriesMetric(string) workqueue.CounterMetric { return noopMetric{} }

// NewQueue is a controller.Options.NewQueue that builds the same priority queue
// controller-runtime builds by default (same rate limiter, same per-controller
// logger), minus its metrics and their 500 ms refresh loop.
func NewQueue(controllerName string, rateLimiter workqueue.TypedRateLimiter[reconcile.Request]) workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return priorityqueue.New("", func(o *priorityqueue.Opts[reconcile.Request]) {
		o.Log = ctrl.Log.WithName("controller").WithValues("controller", controllerName)
		o.RateLimiter = rateLimiter
		o.MetricProvider = NoopMetricsProvider{}
	})
}

// Options returns controller options whose queue keeps no metrics. Callers that
// need other options set them on the returned value.
func Options() controller.Options {
	return controller.Options{NewQueue: NewQueue}
}
