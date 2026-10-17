package servicerange

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"

	"aethermesh.dev/common/crdcheck"
	"aethermesh.dev/common/ctrlqueue"
	commonlog "aethermesh.dev/common/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
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
	// Override is the --service-cidrs value. When it holds any entry it
	// replaces the ServiceCIDR objects and their reconciler is not started.
	// It replaces only that part: the ClusterSetIPs of ServiceImports outside
	// it are still added.
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
// otherwise whatever a ServiceCIDR reconciler registered with mgr discovers,
// plus a /32 for each ClusterSetIP of a ServiceImport that lies outside it.
// Each reconciler is gated on its API being served (ServiceCIDR: Kubernetes
// >= 1.33; ServiceImport: the multicluster.x-k8s.io CRD); when neither is and
// there is no override, no range is known.
//
// An override that is not a list of CIDRs is an error: the agent fails at
// start rather than run with a range nobody asked for.
func Setup(mgr ctrl.Manager, opts Options) error {
	// Checked before the manager is touched, so a bad flag fails first.
	if _, _, err := ParseCIDRs(opts.Override); err != nil {
		return fmt.Errorf("--service-cidrs: %w", err)
	}
	return setup(mgr.GetRESTMapper(), func(r *Reconciler) error {
		r.Client = mgr.GetClient()
		return ctrl.NewControllerManagedBy(mgr).
			For(&networkingv1.ServiceCIDR{}).
			WatchesRawSource(source.Channel(initialReconcile(), &handler.EnqueueRequestForObject{})).
			Named("servicerange").
			WithOptions(ctrlqueue.Options()).
			Complete(r)
	}, func(r *ImportReconciler) error {
		if err := mcsv1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
			return fmt.Errorf("register multicluster.x-k8s.io scheme: %w", err)
		}
		r.Client = mgr.GetClient()
		return ctrl.NewControllerManagedBy(mgr).
			For(&mcsv1alpha1.ServiceImport{}).
			Named("servicerange-imports").
			WithOptions(ctrlqueue.Options()).
			Complete(r)
	}, opts)
}

// initialReconcile returns a source holding exactly one event, so the
// ServiceCIDR reconciler runs once after its cache syncs even when there is no
// ServiceCIDR object to trigger it, and the range is decided (as none).
//
// The xDS server holds its first serve until the sink's first delivery
// (cache.ServiceRangeDelivered), and a surge standby waits for it with no
// bound: a cluster that serves the API with no object in it would otherwise
// never deliver. Going through the controller's own workqueue, rather than
// calling Reconcile from a side goroutine, keeps the re-lists serialized, as
// capture.initialProjection does. Reconcile ignores the request, so the
// object only has to be a valid key.
func initialReconcile() <-chan event.GenericEvent {
	ch := make(chan event.GenericEvent, 1)
	ch <- event.GenericEvent{Object: &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "aether-initial-service-range"}}}
	return ch
}

// serviceImportGVK is the MCS ServiceImport, whose CRD gates the import half.
var serviceImportGVK = schema.GroupVersionKind{Group: mcsv1alpha1.GroupName, Version: "v1alpha1", Kind: "ServiceImport"}

// setup is Setup with the manager reduced to what it uses, so the decision
// between override, discovery and nothing is tested without an apiserver.
func setup(mapper meta.RESTMapper, registerCIDRs func(*Reconciler) error, registerImports func(*ImportReconciler) error, opts Options) error {
	st, err := newState(opts.Log, opts.Meter)
	if err != nil {
		return err
	}
	if err := setupCIDRs(mapper, registerCIDRs, st, opts); err != nil {
		return err
	}

	served, err := crdcheck.Present(mapper, serviceImportGVK)
	if err != nil {
		return err
	}
	if !served {
		// Most clusters run no MCS implementation: not a warning.
		st.log.Info("the multicluster.x-k8s.io ServiceImport CRD is not installed; no ClusterSetIP joins the Service range (installing it needs an agent restart)")
		return nil
	}
	return registerImports(&ImportReconciler{Sink: opts.Sink, state: st})
}

// setupCIDRs sets up the ServiceCIDR part of the range: the override, the
// ServiceCIDR reconciler, or no range.
func setupCIDRs(mapper meta.RESTMapper, register func(*Reconciler) error, st *state, opts Options) error {
	if len(opts.Override) > 0 {
		ranges, droppedIPv6, err := ParseCIDRs(opts.Override)
		if err != nil {
			return fmt.Errorf("--service-cidrs: %w", err)
		}
		if droppedIPv6 > 0 {
			st.log.Info("ignoring IPv6 entries of --service-cidrs: the Service range is IPv4 only for now", "ignored", droppedIPv6)
		}
		st.setCIDRs(context.Background(), opts.Sink, ranges, "flag")
		return nil
	}

	served, err := crdcheck.Present(mapper, networkingv1.SchemeGroupVersion.WithKind("ServiceCIDR"))
	if err != nil {
		return err
	}
	if !served {
		st.log.Info("the networking.k8s.io/v1 ServiceCIDR API is not served (Kubernetes < 1.33); set agent.serviceCIDRs to give the agent the Service range")
		st.setCIDRs(context.Background(), opts.Sink, nil, "none")
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
	r.state.setCIDRs(ctx, r.Sink, Canonicalize(parsed), "ServiceCIDR")
	return reconcile.Result{}, nil
}

// state is what all sources share: the two parts of the range, the last set
// handed to the sink, the gauge and the one-WARN rule. The ServiceCIDR and
// ServiceImport reconcilers run on their own goroutines; mu orders them.
type state struct {
	log   *slog.Logger
	known atomic.Int64

	mu sync.Mutex
	// warned is set when the unknown-range WARN has been logged, and cleared
	// when a range becomes known, so a range that is lost again warns again.
	warned bool
	// cidrs is the ServiceCIDR part (the objects or the override), canonical.
	cidrs []netip.Prefix
	// cidrsDecided is set by the first setCIDRs. Nothing reaches the sink
	// before it: the first delivery opens the xDS server's first serve, and
	// must carry the Service range, not a set of ClusterSetIPs alone.
	cidrsDecided bool
	// clusterSetIPs are the IPv4 ClusterSetIPs of the ServiceImports, sorted
	// and deduplicated, whether or not cidrs covers them.
	clusterSetIPs []netip.Addr
	// published is set once the sink has been handed a set; last is that set.
	published bool
	last      []netip.Prefix
}

func newState(log *slog.Logger, meter metric.Meter) (*state, error) {
	st := &state{log: commonlog.Named(log, "servicerange")}
	if meter == nil {
		meter = otel.Meter(meterName)
	}
	if _, err := meter.Int64ObservableGauge("aether.agent.service_range.known",
		metric.WithDescription("1 when the agent knows the cluster's IPv4 Service range (from ServiceCIDR objects or --service-cidrs), 0 when it does not; at 0 every connection to a non-mesh ClusterIP Service is passed through to its original destination unchanged (proposal 044). ClusterSetIPs of ServiceImports do not count"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(st.known.Load())
			return nil
		})); err != nil {
		return nil, fmt.Errorf("aether.agent.service_range.known: %w", err)
	}
	return st, nil
}

// setCIDRs replaces the ServiceCIDR part of the range, records whether that
// part is known, and hands the sink the combined set if it changed.
func (s *state) setCIDRs(ctx context.Context, sink Sink, ranges []netip.Prefix, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cidrs = ranges
	s.cidrsDecided = true
	s.flushLocked(sink)

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

// setClusterSetIPs replaces the ServiceImport part of the range and hands the
// sink the combined set if it changed.
func (s *state) setClusterSetIPs(ctx context.Context, sink Sink, ips []netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Equal(s.clusterSetIPs, ips) {
		return
	}
	s.clusterSetIPs = ips
	s.log.DebugContext(ctx, "ServiceImport ClusterSetIPs changed", "clusterSetIPs", len(ips))
	s.flushLocked(sink)
}

// flushLocked hands the sink the combined range, unless the ServiceCIDR part
// is not decided yet or it is the set the sink already has. The first call
// after the ServiceCIDR part is decided always hands it one, nil included, so
// the sink learns that no range is known. Called with mu held, so two
// reconcilers never deliver out of order.
func (s *state) flushLocked(sink Sink) {
	if !s.cidrsDecided {
		return
	}
	set := Combine(s.cidrs, s.clusterSetIPs)
	if s.published && slices.Equal(set, s.last) {
		return
	}
	s.published = true
	s.last = set
	sink.SetServiceRanges(set)
}
