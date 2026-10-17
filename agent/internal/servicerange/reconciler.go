package servicerange

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"

	"aethermesh.dev/common/crdcheck"
	"aethermesh.dev/common/ctrlqueue"
	commonlog "aethermesh.dev/common/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/agent-service-range"

// Sink receives the Service range. Implemented by the snapshot cache.
type Sink interface {
	// SetServiceRanges replaces the Service range. The slice is canonical
	// (Canonicalize) and may be nil: no range is known.
	SetServiceRanges(ranges []netip.Prefix)
}

// Options configures Setup.
type Options struct {
	// Override is the --service-cidrs value. When it holds any entry it wins
	// over discovery and the ServiceCIDR reconciler is not started.
	Override []string
	// Sink receives the range.
	Sink Sink
	// Log is the agent's logger.
	Log *slog.Logger
	// Meter registers aether.agent.service_range.known. Nil uses the global
	// meter provider.
	Meter metric.Meter
}

// Setup delivers the Service range to the sink: the override when it is set,
// otherwise whatever a ServiceCIDR reconciler registered with mgr discovers.
// The reconciler is gated on the API being served (Kubernetes >= 1.33); when
// it is not, and there is no override, no range is known.
//
// An override that is not a list of CIDRs is an error: the agent fails at
// start rather than run with a range nobody asked for.
func Setup(mgr ctrl.Manager, opts Options) error {
	if len(opts.Override) > 0 {
		// The override needs neither the API nor the manager.
		return setup(nil, nil, opts)
	}
	return setup(mgr.GetRESTMapper(), func(r *Reconciler) error {
		r.Client = mgr.GetClient()
		return ctrl.NewControllerManagedBy(mgr).
			For(&networkingv1.ServiceCIDR{}).
			Named("servicerange").
			WithOptions(ctrlqueue.Options()).
			Complete(r)
	}, opts)
}

// setup is Setup with the manager reduced to what it uses, so the decision
// between override, discovery and nothing is tested without an apiserver.
func setup(mapper meta.RESTMapper, register func(*Reconciler) error, opts Options) error {
	st, err := newState(opts.Log, opts.Meter)
	if err != nil {
		return err
	}
	if len(opts.Override) > 0 {
		ranges, droppedIPv6, err := ParseCIDRs(opts.Override)
		if err != nil {
			return fmt.Errorf("--service-cidrs: %w", err)
		}
		if droppedIPv6 > 0 {
			st.log.Info("ignoring IPv6 entries of --service-cidrs: the Service range is IPv4 only for now", "ignored", droppedIPv6)
		}
		st.publish(context.Background(), opts.Sink, ranges, "flag")
		return nil
	}

	served, err := crdcheck.Present(mapper, networkingv1.SchemeGroupVersion.WithKind("ServiceCIDR"))
	if err != nil {
		return err
	}
	if !served {
		st.log.Info("the networking.k8s.io/v1 ServiceCIDR API is not served (Kubernetes < 1.33); set agent.serviceCIDRs to give the agent the Service range")
		st.publish(context.Background(), opts.Sink, nil, "none")
		return nil
	}
	return register(&Reconciler{Sink: opts.Sink, state: st})
}

// Reconciler watches the cluster's ServiceCIDR objects and hands the sink the
// IPv4 blocks of all of them on any change. Level-based: each reconcile
// re-lists, so a second ServiceCIDR added at runtime, or one removed, is
// picked up without delta tracking.
type Reconciler struct {
	client.Client

	Sink  Sink
	state *state
}

// Reconcile re-lists every ServiceCIDR and replaces the sink's range.
//
// A ServiceCIDR that is being deleted is kept until it is gone. The apiserver
// holds its finalizer (and reports Ready=False, reason Terminating) for as long
// as a ClusterIP allocated from it is not covered by another ServiceCIDR, so
// its block can still hold live Services; dropping it early would send their
// traffic down the passthrough. A ServiceCIDR not yet Ready is kept too: a
// block in the range that no Service owns costs nothing, a Service outside it
// loses its routing.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &networkingv1.ServiceCIDRList{}
	if err := r.List(ctx, list); err != nil {
		return reconcile.Result{}, err
	}
	var parsed []netip.Prefix
	for i := range list.Items {
		for _, s := range list.Items[i].Spec.CIDRs {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				// The apiserver validates spec.cidrs; nothing to retry.
				r.state.log.WarnContext(ctx, "ignoring a ServiceCIDR block that is not a CIDR",
					"serviceCIDR", list.Items[i].Name, "cidr", s, "error", err)
				continue
			}
			parsed = append(parsed, p)
		}
	}
	r.state.publish(ctx, r.Sink, Canonicalize(parsed), "ServiceCIDR")
	return reconcile.Result{}, nil
}

// state is what both sources share: the gauge and the one-WARN rule.
type state struct {
	log   *slog.Logger
	known atomic.Int64

	mu sync.Mutex
	// warned is set when the unknown-range WARN has been logged, and cleared
	// when a range becomes known, so a range that is lost again warns again.
	warned bool
}

func newState(log *slog.Logger, meter metric.Meter) (*state, error) {
	st := &state{log: commonlog.Named(log, "servicerange")}
	if meter == nil {
		meter = otel.Meter(meterName)
	}
	if _, err := meter.Int64ObservableGauge("aether.agent.service_range.known",
		metric.WithDescription("1 when the agent knows the cluster's IPv4 Service range (from ServiceCIDR objects or --service-cidrs), 0 when it does not; at 0 every connection to a non-mesh Service is passed through to its original destination unchanged (proposal 044)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(st.known.Load())
			return nil
		})); err != nil {
		return nil, fmt.Errorf("aether.agent.service_range.known: %w", err)
	}
	return st, nil
}

// publish hands ranges to the sink and records whether a range is known.
func (s *state) publish(ctx context.Context, sink Sink, ranges []netip.Prefix, source string) {
	sink.SetServiceRanges(ranges)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ranges) > 0 {
		if s.known.Swap(1) != 1 {
			s.log.InfoContext(ctx, "Service range known", "source", source, "ranges", fmt.Sprint(ranges))
		}
		s.warned = false
		return
	}
	s.known.Store(0)
	if !s.warned {
		s.warned = true
		s.log.WarnContext(ctx, "no IPv4 Service range known (no ServiceCIDR object and no --service-cidrs / agent.serviceCIDRs): connections to non-mesh Services keep going to their original destination",
			"source", source)
	}
}
