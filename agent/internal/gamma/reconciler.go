// Package gamma contains the node agent's GAMMA controller: it watches Gateway API
// HTTPRoutes and GRPCRoutes attached to a Service (parentRef kind=Service) — plus
// the ReferenceGrants that authorise cross-namespace backendRefs and the HTTPFilter
// extensions those rules attach (proposal 025) — and projects their L7 rules into
// the node proxy's routing (proposal 018, Phase 2 — east-west). The projection
// feeds BOTH route paths the proxy serves: the outbound per-service listeners and
// the transparent-capture listener's cap_http virtual hosts. Routing and mTLS are
// unchanged; the routes add canary/header/timeout vocabulary to a path the proxy
// already serves.
package gamma

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"aethermesh.dev/agent/internal/gatewaystatus"
	"aethermesh.dev/agent/internal/xds/proxy"
	configprotov1 "aethermesh.dev/api/aether/config/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/crdcheck"
	"aethermesh.dev/common/ctrlqueue"
	"aethermesh.dev/common/gammaproject"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/serviceref"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// RouteSink receives the projected per-service GAMMA rules (the snapshot cache).
type RouteSink interface {
	SetServiceRoutes(routes map[string][]proxy.GammaRoute)
	// SetServiceChainFilters receives the service-wide ALWAYS-ON extension filters
	// (proposal 025 M4 CHAIN scope), keyed by "<ns>/<svc>": at most one per service,
	// from a CHAIN-scope HTTPFilter with a same-namespace Service targetRef. Enabled
	// at the service's capture vhost.
	SetServiceChainFilters(filters map[string]proxy.ExtensionFilter)
	// SetServiceInboundFilters receives the destination-side (INBOUND scope, 027 M3)
	// filters, keyed by "<ns>/<svc>": enabled on the target service's own pods'
	// inbound listeners. At most one per service.
	SetServiceInboundFilters(filters map[string]proxy.ExtensionFilter)
	// SetRouteTargetPorts receives the real Service port(s) of each route target
	// (proposal 023 M2), keyed by the same "<ns>/<svc>" route-target key as
	// SetServiceRoutes. Sourced from the HTTPRoute/GRPCRoute parentRef port; lets the
	// cap_http vhost host-match a client dialing the target's REAL port.
	SetRouteTargetPorts(ports map[string][]uint32)
	// SetServicePortMaps receives the port map of every Service a route names
	// as its parent or as a backend and that the registrar did not generate
	// (proposal 043, "A Service is a port map"), keyed "<ns>/<svc>": Service
	// port -> the pod port its targetPort names, 0 when the targetPort is a
	// name. A route's ports are ports of the SERVICE; the mesh resolves them
	// to pod ports through this.
	SetServicePortMaps(portMaps map[string]map[uint32]uint32)
}

// Reconciler watches HTTPRoutes (parentRef=Service) cluster-wide and projects, on
// any change, the complete service→rules map into the cache. Level-based: each
// reconcile re-lists, so adds/updates/deletes converge without delta tracking.
//
// Phase 2 treats every Service-parented HTTPRoute as a producer route (applies to
// all clients of that service); per-namespace consumer overrides are a follow-up.
type Reconciler struct {
	client.Client

	// APIReader reads the API directly, without a cache. Used only to read
	// the EndpointSlices of a route-named Service whose targetPort is a name
	// (resolveNamedTargetPorts). Nil disables that resolution.
	APIReader client.Reader

	// PodPorts says what the pods of a user's Service declare, for a route's
	// status (podports.go). Nil: such a port is reported as not checked.
	PodPorts PodPorts
	// PodPortsChanged delivers an event each time what PodPorts answers has
	// changed (the cache's OnPodPortsChanged): every route's status is then
	// judged again. Nil: the status follows only what the reconciler watches.
	PodPortsChanged <-chan event.GenericEvent

	// Sink receives the projected per-service rules (the snapshot cache).
	Sink RouteSink
	// MeshDomain resolves a backend Service to its data-plane cluster name.
	MeshDomain string
	Log        *slog.Logger

	// httpFilterEnabled is set in SetupWithManager when the HTTPFilter CRD (proposal
	// 025) is present. When false, the reconciler neither watches nor lists HTTPFilters
	// (the escape hatch is inert) — so a missing/not-yet-installed CRD degrades
	// gracefully instead of crashing the manager on a failed informer sync.
	httpFilterEnabled bool
	// Now is the clock of the status writer's back-off; nil is time.Now.
	Now func() time.Time

	// After runs f after d; nil is time.AfterFunc. It is the timer that keeps
	// a held-back status's wait when a reconcile ends in an error (wakeIn).
	After func(d time.Duration, f func())

	// namedTargets are the Services a route names that have a named
	// targetPort, as of the last reconcile (setNamedTargets).
	namedMu      sync.Mutex
	namedTargets map[string]struct{}
	namedNow     map[string]struct{}
	// resolutions: what the EndpointSlices of each concerned Service said of
	// its named targetPorts when last read; sliceChanged: the Services a
	// slice event was seen for since (namedTargetPorts).
	resolutions  map[string]resolution
	sliceChanged map[string]bool
	// warned / warnedNow: the unresolved named targetPorts logged at the
	// last reconcile and at the running one (warnUnresolvedOnce).
	warned    map[string]struct{}
	warnedNow map[string]struct{}

	wakeOnce sync.Once
	wake     chan event.GenericEvent
	wakeMu   sync.Mutex
	wakeAt   time.Time

	// contested remembers, per route, what this agent last wanted its status
	// to be and when it wrote it, to hold back when another writer keeps
	// overwriting it (contest.go).
	contested contestLedger

	// grpcRouteEnabled / referenceGrantEnabled likewise gate the optional Gateway API
	// types on CRD presence (proposal 031): a partial Gateway API install degrades the
	// affected feature with a warning instead of wedging the manager.
	grpcRouteEnabled      bool
	referenceGrantEnabled bool
}

// SetupWithManager registers the reconciler to watch HTTPRoutes and GRPCRoutes. Both
// feed the same per-service rule map; any change re-lists both (level-based), so a
// single fixed request enqueued for either type is enough.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "gamma")
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})

	// Gateway API + HTTPFilter are EXTERNAL CRDs: watching a type whose CRD is
	// absent makes the cache fail to sync and HARD-CRASHES the manager (the
	// install-ordering crashloop). Gate every type on presence (proposal 031):
	// HTTPRoute is the floor — without it GAMMA is skipped entirely, with a
	// warning; GRPCRoute / ReferenceGrant / HTTPFilter degrade individually.
	// Detection is setup-time: installing a CRD later needs an agent restart.
	mapper := mgr.GetRESTMapper()
	httpRoutePresent, err := crdcheck.Present(mapper, gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"))
	if err != nil {
		return err
	}
	if !httpRoutePresent {
		r.Log.Warn("Gateway API CRDs not present; GAMMA east-west routing disabled until they are installed and the agent restarts")
		return nil
	}
	if r.grpcRouteEnabled, err = crdcheck.Present(mapper, gatewayv1.SchemeGroupVersion.WithKind("GRPCRoute")); err != nil {
		return err
	} else if !r.grpcRouteEnabled {
		r.Log.Warn("GRPCRoute CRD not present; gRPC method routing disabled until it is installed and the agent restarts")
	}
	// ReferenceGrant (v1beta1, the served storage version): a change to any grant
	// can flip a cross-namespace backendRef between permitted and RefNotPermitted,
	// so re-project + re-status on every grant change. Cluster-wide cache (post-#323).
	if r.referenceGrantEnabled, err = crdcheck.Present(mapper, gatewayv1beta1.SchemeGroupVersion.WithKind("ReferenceGrant")); err != nil {
		return err
	} else if !r.referenceGrantEnabled {
		r.Log.Warn("ReferenceGrant CRD not present; cross-namespace backendRefs stay RefNotPermitted until it is installed and the agent restarts")
	}
	if r.httpFilterEnabled, err = crdcheck.Present(mapper, configapisv1.GroupVersion.WithKind(configapisv1.HTTPFilterKind)); err != nil {
		return err
	} else if !r.httpFilterEnabled {
		r.Log.Warn("HTTPFilter CRD not present; proxy-extension escape hatch (025) disabled until the CRD is installed and the agent restarts")
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{})
	for _, w := range r.secondaryWatches() {
		opts := []builder.WatchesOption{builder.WithPredicates(w.predicates...)}
		if w.metadataOnly {
			opts = append(opts, builder.OnlyMetadata)
		}
		b = b.Watches(w.object, enqueueAll, opts...)
	}
	// The reconciler's own timer (wakeIn).
	b = b.WatchesRawSource(source.Channel((<-chan event.GenericEvent)(r.wakeEvents()), enqueueAll))
	if r.PodPortsChanged != nil {
		// What the pods of a service declare is not an API object this
		// reconciler watches: the cache says when its answer changed. That
		// includes the refresh a reconcile itself causes, by making a route's
		// services dependencies of the node: the status is judged again when
		// their rows have landed, not on a timer.
		b = b.WatchesRawSource(source.Channel(r.PodPortsChanged, enqueueAll))
	}
	return b.Named("gamma").WithOptions(controllerOptions()).Complete(r)
}

// maxRetryDelay is the longest the controller waits before it runs a
// reconcile that failed again.
var maxRetryDelay = 30 * time.Second

// retryLimiter is the back-off of a failed reconcile: from 5 ms, doubling, to
// maxRetryDelay. controller-runtime's default grows to 1000 s. A reconcile
// fails when the API cannot be read or written (a status write, the
// EndpointSlices of a Service with a named targetPort), and while it does,
// what it could not read is refused: after an API outage the node would have
// kept refusing those ports for up to a quarter of an hour after the API was
// back.
func retryLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, maxRetryDelay)
}

// controllerOptions are the options the gamma controller is registered with.
func controllerOptions() controller.Options {
	opts := ctrlqueue.Options()
	opts.RateLimiter = retryLimiter()
	return opts
}

// secondaryWatch is one kind the reconciler watches besides HTTPRoute, and
// the predicates that filter its events.
type secondaryWatch struct {
	object     client.Object
	predicates []predicate.Predicate
	// metadataOnly: the informer holds the objects' metadata and nothing
	// else of them.
	metadataOnly bool
}

// secondaryWatches returns what the reconciler watches besides HTTPRoute,
// each of which re-projects every route. It is a function of its own so that
// the set can be held by a test: a kind that is listed in Reconcile and not
// watched here is read once and never again.
func (r *Reconciler) secondaryWatches() []secondaryWatch {
	var out []secondaryWatch
	if r.grpcRouteEnabled {
		out = append(out, secondaryWatch{object: &gatewayv1.GRPCRoute{}})
	}
	if r.referenceGrantEnabled {
		out = append(out, secondaryWatch{object: &gatewayv1beta1.ReferenceGrant{}})
	}
	if r.httpFilterEnabled {
		// A change to a referenced HTTPFilter's opaque config must re-project the routes
		// that reference it.
		out = append(out, secondaryWatch{object: &configapisv1.HTTPFilter{}})
	}
	// A Service is a port map (proposal 043): a route's ports are resolved
	// through it, and so is a port-less request to a service a Service
	// fronts. A Service whose ports change must reach the cache. Only a
	// change of ports or of ownership counts; Services change status and
	// annotations far more often than that.
	out = append(out, secondaryWatch{object: &corev1.Service{}, predicates: []predicate.Predicate{servicePortsChanged()}})
	// The EndpointSlices of the Services a route names that have a named
	// targetPort: when one appears or changes, the name may resolve.
	return append(out, secondaryWatch{object: &discoveryv1.EndpointSlice{}, predicates: []predicate.Predicate{r.namedTargetSlices()}, metadataOnly: true})
}

// Reconcile re-lists every HTTPRoute and GRPCRoute, keeps those attached to a Service,
// and replaces the cache's per-service rule set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	httpList, grpcList, grants, err := r.listGammaResources(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	httpFilters, err := r.listHTTPFilters(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	specs := httpFilterSpecs(httpFilters)
	chainFilters, inboundFilters := r.buildScopedFilters(specs)

	routes, portSets := r.projectAllRoutes(httpList, grpcList, grants, httpFilters, specs)
	routeTargetPorts := buildRouteTargetPorts(portSets)
	portMaps, facts, unresolvedErr, err := r.servicePortMaps(ctx, routes, filterTargets(chainFilters, inboundFilters))
	if err != nil {
		return reconcile.Result{}, err
	}

	// The port maps first: the rules are placed and their backends resolved
	// through them.
	r.Sink.SetServicePortMaps(portMaps)
	r.Sink.SetServiceRoutes(routes)
	r.Sink.SetServiceChainFilters(chainFilters)
	r.Sink.SetServiceInboundFilters(inboundFilters)
	r.Sink.SetRouteTargetPorts(routeTargetPorts)
	r.Log.DebugContext(ctx, "projected gamma service routes",
		"httpRoutes", len(httpList.Items), "grpcRoutes", len(grpcList.Items), "services", len(routes))

	// Publish Gateway API status: for every Service-parented route we own, write an
	// Accepted + ResolvedRefs RouteParentStatus per Service parentRef. Status write
	// failures are logged but don't fail the reconcile (the data plane is already
	// projected); the next reconcile retries.
	//
	// A status write that fails is an error of the reconcile, which is then
	// run again (a conflict aside: the route changed, and that is an event).
	// The status is judged on events and on nothing else, so a failed write
	// that was only logged left the status as it was, True while the tables
	// answered 500, until some other event came by. And a status another
	// writer keeps overwriting is written again later, not at once: the
	// reconcile asks to be run when the earliest of those is due.
	seen := map[string]struct{}{}
	wait, err := r.writeHTTPRouteStatuses(ctx, httpList, grants, facts, seen)
	grpcWait, grpcErr := r.writeGRPCRouteStatuses(ctx, grpcList, grants, facts, seen)
	r.contested.keep(seen)
	held := soonest(wait, grpcWait)
	if err := errors.Join(err, grpcErr, unresolvedErr); err != nil {
		// The controller ignores a RequeueAfter beside an error and runs the
		// reconciler again on its own back-off. A route that is being held
		// back must not wait for that: its wait is kept by a timer of the
		// reconciler's own. (The ten-minute re-read needs none: every
		// reconcile, the retries of this error among them, reads what is
		// due.)
		r.wakeIn(held)
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: soonest(held, r.namedResyncDue())}, nil
}

// wakeIn has the reconciler run again in d (zero: nothing to do), whatever
// the controller's queue does with the request meanwhile. One timer at a
// time: a later call with a longer wait than the one pending adds nothing.
func (r *Reconciler) wakeIn(d time.Duration) {
	if d <= 0 {
		return
	}
	at := r.now().Add(d)
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	if !r.wakeAt.IsZero() && !at.Before(r.wakeAt) {
		return
	}
	r.wakeAt = at
	after := r.After
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(d, func() {
		r.wakeMu.Lock()
		if r.wakeAt.Equal(at) {
			r.wakeAt = time.Time{}
		}
		r.wakeMu.Unlock()
		select {
		case r.wakeEvents() <- event.GenericEvent{}:
		default:
		}
	})
}

// wakeEvents is the channel wakeIn's timer sends on; the controller watches
// it (SetupWithManager).
func (r *Reconciler) wakeEvents() chan event.GenericEvent {
	r.wakeOnce.Do(func() { r.wake = make(chan event.GenericEvent, 1) })
	return r.wake
}

// soonest returns the shorter of two waits, where zero is "none".
func soonest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// listGammaResources lists HTTPRoutes, GRPCRoutes, and ReferenceGrants (all CRD-gated).
func (r *Reconciler) listGammaResources(ctx context.Context) (*gatewayv1.HTTPRouteList, *gatewayv1.GRPCRouteList, []gatewayv1beta1.ReferenceGrant, error) {
	httpList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, httpList); err != nil {
		return nil, nil, nil, err
	}
	// Optional types (CRD-gated in SetupWithManager, proposal 031): listing a
	// kind whose CRD is absent errors every reconcile, so skip when disabled.
	grpcList := &gatewayv1.GRPCRouteList{}
	if r.grpcRouteEnabled {
		if err := r.List(ctx, grpcList); err != nil {
			return nil, nil, nil, err
		}
	}
	// Cluster-wide ReferenceGrants gate cross-namespace backendRefs.
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if r.referenceGrantEnabled {
		if err := r.List(ctx, grantList); err != nil {
			return nil, nil, nil, err
		}
	}
	return httpList, grpcList, grantList.Items, nil
}

// listHTTPFilters lists HTTPFilter objects indexed by "<ns>/<name>", or returns an
// empty map when the CRD is absent. HTTPFilters (proposal 025) are gated on
// httpFilterEnabled (set in SetupWithManager).
func (r *Reconciler) listHTTPFilters(ctx context.Context) (map[string]*configapisv1.HTTPFilter, error) {
	httpFilters := map[string]*configapisv1.HTTPFilter{}
	if !r.httpFilterEnabled {
		return httpFilters, nil
	}
	// HTTPFilters (config.aether.io, proposal 025) referenced by route ExtensionRefs,
	// indexed by "<ns>/<name>". ExtensionRef is same-namespace (a LocalObjectReference),
	// so no ReferenceGrant applies. Only listed when the CRD is present (gated in
	// SetupWithManager); otherwise the escape hatch is inert and the map stays empty.
	httpFilterList := &configapisv1.HTTPFilterList{}
	if err := r.List(ctx, httpFilterList); err != nil {
		return nil, err
	}
	for i := range httpFilterList.Items {
		hf := &httpFilterList.Items[i]
		httpFilters[hf.Namespace+"/"+hf.Name] = hf
	}
	return httpFilters, nil
}

// buildScopedFilters resolves per-service CHAIN and INBOUND scope extension filters
// from the HTTPFilter specs (proposal 025 M4 / 027 M3).
func (r *Reconciler) buildScopedFilters(specs map[string]*configprotov1.HTTPFilterSpec) (chainFilters, inboundFilters map[string]proxy.ExtensionFilter) {
	// Service-wide always-on filters (M4 CHAIN scope) + destination-side INBOUND
	// filters (027 M3): resolve per targeted service.
	chainFilters = map[string]proxy.ExtensionFilter{}
	inboundFilters = map[string]proxy.ExtensionFilter{}
	for key, spec := range specs {
		scope := spec.GetScope()
		if scope != configprotov1.HTTPFilterSpec_SCOPE_CHAIN && scope != configprotov1.HTTPFilterSpec_SCOPE_INBOUND {
			continue
		}
		ns, _, _ := strings.Cut(key, "/")
		r.resolveTargetRefFilters(ns, spec, specs, scope, chainFilters, inboundFilters)
	}
	return chainFilters, inboundFilters
}

// resolveTargetRefFilters resolves CHAIN/INBOUND filters for each targetRef in the spec.
func (r *Reconciler) resolveTargetRefFilters(ns string, spec *configprotov1.HTTPFilterSpec, specs map[string]*configprotov1.HTTPFilterSpec, scope configprotov1.HTTPFilterSpec_Scope, chainFilters, inboundFilters map[string]proxy.ExtensionFilter) {
	for _, t := range spec.GetTargetRefs() {
		if (t.GetGroup() != "" && t.GetGroup() != "core") || t.GetKind() != "Service" {
			continue
		}
		svcKey := ns + "/" + t.GetName()
		// Deterministic pick delegated to the shared projector (name-sorted).
		if scope == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
			if ef := gammaproject.ServiceChainFilter(svcKey, specs); ef != nil {
				chainFilters[svcKey] = proxy.ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()}
			}
		} else {
			if ef := gammaproject.ServiceInboundFilter(svcKey, specs); ef != nil {
				inboundFilters[svcKey] = proxy.ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()}
			}
		}
	}
}

// projectAllRoutes projects HTTP and GRPC routes into per-service route maps.
// It returns the route map and port sets for target-port collection.
func (r *Reconciler) projectAllRoutes(
	httpList *gatewayv1.HTTPRouteList,
	grpcList *gatewayv1.GRPCRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
) (map[string][]proxy.GammaRoute, map[string]map[uint32]struct{}) {
	routes := map[string][]proxy.GammaRoute{}
	// routeTargetPorts: the real Service port(s) each route target is addressed on
	// (proposal 023 M2), accumulated from the parentRef ports. A target may be
	// addressed on several ports across routes; collect the distinct set.
	portSets := map[string]map[uint32]struct{}{}
	r.projectHTTPRouteItems(httpList, grants, httpFilters, specs, routes, portSets)
	r.projectGRPCRouteItems(grpcList, grants, httpFilters, specs, routes, portSets)
	return routes, portSets
}

// projectHTTPRouteItems projects each HTTPRoute item into the routes and portSets maps.
func (r *Reconciler) projectHTTPRouteItems(
	httpList *gatewayv1.HTTPRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
	routes map[string][]proxy.GammaRoute,
	portSets map[string]map[uint32]struct{},
) {
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, p := range gammaproject.ServiceParents(hr.Spec.ParentRefs, hr.Namespace) {
			addToPortSets(portSets, p.Key, p.Port)
			svcFilters := gammaproject.ServiceFilters(p.Key, specs)
			for _, rule := range hr.Spec.Rules {
				gr := r.buildGammaRoute(rule, hr.Namespace, "HTTPRoute", grants, httpFilters, svcFilters)
				refs := make([]gatewayv1.BackendObjectReference, 0, len(rule.BackendRefs))
				plain := make([]gatewayv1.BackendRef, 0, len(rule.BackendRefs))
				for _, b := range rule.BackendRefs {
					refs = append(refs, b.BackendObjectReference)
					plain = append(plain, b.BackendRef)
				}
				gr = withDropped(gr, plain, hr.Namespace, "HTTPRoute", grants)
				routes[p.Key] = append(routes[p.Key], withPorts(gr, p.Port, gammaproject.BackendRefPorts(refs, hr.Namespace, "HTTPRoute", grants)))
			}
		}
	}
}

// projectGRPCRouteItems projects each GRPCRoute item into the routes and portSets maps.
func (r *Reconciler) projectGRPCRouteItems(
	grpcList *gatewayv1.GRPCRouteList,
	grants []gatewayv1beta1.ReferenceGrant,
	httpFilters map[string]*configapisv1.HTTPFilter,
	specs map[string]*configprotov1.HTTPFilterSpec,
	routes map[string][]proxy.GammaRoute,
	portSets map[string]map[uint32]struct{},
) {
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, p := range gammaproject.ServiceParents(gr.Spec.ParentRefs, gr.Namespace) {
			addToPortSets(portSets, p.Key, p.Port)
			svcFilters := gammaproject.ServiceFilters(p.Key, specs)
			for _, rule := range gr.Spec.Rules {
				projected := r.buildGammaRouteFromGRPC(rule, gr.Namespace, "GRPCRoute", grants, httpFilters, svcFilters)
				refs := make([]gatewayv1.BackendObjectReference, 0, len(rule.BackendRefs))
				plain := make([]gatewayv1.BackendRef, 0, len(rule.BackendRefs))
				for _, b := range rule.BackendRefs {
					refs = append(refs, b.BackendObjectReference)
					plain = append(plain, b.BackendRef)
				}
				projected = withDropped(projected, plain, gr.Namespace, "GRPCRoute", grants)
				routes[p.Key] = append(routes[p.Key], withPorts(projected, p.Port, gammaproject.BackendRefPorts(refs, gr.Namespace, "GRPCRoute", grants)))
			}
		}
	}
}

// withDropped records on a projected rule the backendRefs the projection
// dropped (another kind, or not permitted by a ReferenceGrant): invalid
// backendRefs, whose share of the rule the proxy answers 500.
func withDropped(rule proxy.GammaRoute, refs []gatewayv1.BackendRef, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant) proxy.GammaRoute {
	n, weight := gammaproject.DroppedBackendRefs(refs, routeNamespace, routeKind, grants)
	rule.DroppedBackends = n > 0
	rule.DroppedBackendWeight = weight
	return rule
}

// withPorts returns a projected rule carrying the two ports the shared
// projection does not: the parentRef's port (0 = none: the rule applies to
// every port of the parent Service) and each backendRef's port, both ports of
// a SERVICE (proposal 043). backendPorts is gammaproject.BackendRefPorts over
// the rule's backendRefs, so element i belongs to backend i; a length that
// does not match leaves the backends without a port rather than guess.
func withPorts(rule proxy.GammaRoute, parentPort uint32, backendPorts []uint32) proxy.GammaRoute {
	rule.ParentPort = parentPort
	if len(backendPorts) != len(rule.Backends) {
		return rule
	}
	for i := range rule.Backends {
		rule.Backends[i].Port = backendPorts[i]
	}
	return rule
}

// servicePortsChanged passes the Service events that can change a port map:
// a Service appearing or going, and an update that changes its ports or
// whether the registrar owns it.
func servicePortsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, okOld := e.ObjectOld.(*corev1.Service)
			n, okNew := e.ObjectNew.(*corev1.Service)
			if !okOld || !okNew {
				return true
			}
			return !equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports) ||
				o.Labels[aetherlabels.LabelMeshService] != n.Labels[aetherlabels.LabelMeshService]
		},
	}
}

// servicePortMaps reads the port map of EVERY Service of the cluster that the
// registrar did not generate (proposal 043, "A Service is a port map"), from
// the informer the Service watch keeps.
//
// A Service the registrar generated is left out: it exposes its pods' ports
// unchanged, and the cache derives those from the registry. So is a Service
// that does not exist: the name is then addressed by its pods' own ports.
//
// A numeric targetPort resolves to that pod port, and an unset one to the
// Service port (the API server's own default). A NAMED targetPort is resolved
// through the Service's EndpointSlices for a Service a route names
// (resolveNamedTargetPorts); one that cannot be resolved maps to 0, a
// reference to it keeps the service's declared port, and it is logged.
//
// It also returns, for every Service a route names that exists (generated or
// not), what the Service says about its real ports: the facts a route's
// status is judged by (portstatus.go).
//
// unresolvedErr is not fatal to the reconcile: the port maps and the facts
// are complete, with the named targetPorts whose EndpointSlices could not be
// read left unresolved, which is refused. The reconcile projects them and
// returns the error, so that it is run again.
func (r *Reconciler) servicePortMaps(ctx context.Context, routes map[string][]proxy.GammaRoute, also map[string]struct{}) (portMaps map[string]map[uint32]uint32, _ map[string]portFacts, unresolvedErr, err error) {
	out := map[string]map[uint32]uint32{}
	facts := map[string]portFacts{}
	r.beginNamedTargets()
	var unresolved []error
	named := map[string]struct{}{}
	for _, key := range routeServiceKeys(routes) {
		named[key] = struct{}{}
	}
	// EVERY Service, not only the ones a route names: a Service is a port map
	// whether or not a route exists (a port-less request to a mesh service
	// fronted by "port: 80, targetPort: 8080" is port 80 of it), and a port
	// map that came and went with a route changed what a request meant when
	// the route was deleted. The list is read from the informer this
	// reconciler already keeps for its Service watch, so it costs no API call
	// and no memory that was not already held.
	var services corev1.ServiceList
	if err := r.List(ctx, &services); err != nil {
		return nil, nil, nil, err
	}
	for i := range services.Items {
		svc := &services.Items[i]
		key := serviceref.New(svc.Namespace, svc.Name).Key()
		_, routed := named[key]
		m, user, err := r.servicePortMapAndFacts(ctx, svc, key, routed, also, facts)
		if user {
			out[key] = m
		}
		unresolved = append(unresolved, err)
	}
	r.endNamedTargets()
	// A service a route names that has no Service object: pods can register
	// before the registrar has generated their Service. The data plane judges
	// a reference to it by the pods' own ports, so the status does too
	// (portFacts.absent).
	for key := range named {
		if _, exists := facts[key]; !exists {
			facts[key] = r.withPods(ctx, key, portFacts{absent: true, ports: map[uint32]bool{}}, nil)
		}
	}
	return out, facts, errors.Join(unresolved...), nil
}

// filterTargets returns the Services a service-wide filter targets.
func filterTargets(filters ...map[string]proxy.ExtensionFilter) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range filters {
		for key := range m {
			out[key] = struct{}{}
		}
	}
	return out
}

// The Services whose EndpointSlices the reconciler reads (concerned Services
// with a named targetPort) are the only ones whose slice events it reconciles
// for. The set a slice event is checked against is the one of the last
// reconcile PLUS every Service the running reconcile has started to read
// (addNamedTarget, called before the read), and becomes the running one's
// alone when it ends.

// beginNamedTargets starts the set of the reconcile that is running.
func (r *Reconciler) beginNamedTargets() {
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	r.namedNow = map[string]struct{}{}
	r.warnedNow = map[string]struct{}{}
}

// addNamedTarget records that the running reconcile is about to read the
// EndpointSlices of the Service key.
func (r *Reconciler) addNamedTarget(key string) {
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	if r.namedNow == nil {
		r.namedNow = map[string]struct{}{}
	}
	r.namedNow[key] = struct{}{}
	union := make(map[string]struct{}, len(r.namedTargets)+1)
	maps.Copy(union, r.namedTargets)
	union[key] = struct{}{}
	r.namedTargets = union
}

// endNamedTargets makes the running reconcile's set the current one, and
// forgets what was kept of a Service that is no longer in it.
func (r *Reconciler) endNamedTargets() {
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	r.namedTargets, r.namedNow = r.namedNow, nil
	r.warned, r.warnedNow = r.warnedNow, nil
	for key := range r.resolutions {
		if _, concerned := r.namedTargets[key]; !concerned {
			delete(r.resolutions, key)
		}
	}
	for key := range r.sliceChanged {
		if _, concerned := r.namedTargets[key]; !concerned {
			delete(r.sliceChanged, key)
		}
	}
}

// namedResync is how long a kept resolution of a Service's named targetPorts
// is used without being read again (namedTargetPorts).
const namedResync = 10 * time.Minute

// resolution is what the EndpointSlices of one Service said of its named
// targetPorts when they were last read.
type resolution struct {
	// spec names the Service object and its named ports as they were then
	// (namedSpec): another Service of the name, or other ports, is not this.
	spec string
	at   time.Time
	// ports: Service port to pod port, for the names that resolved.
	ports      map[uint32]uint32
	unresolved map[uint32]unresolvedTarget
}

// namedSpec names a Service object and its named targetPorts.
func namedSpec(svc *corev1.Service, namedPorts []corev1.ServicePort) string {
	var b strings.Builder
	b.WriteString(string(svc.UID))
	for _, sp := range namedPorts {
		fmt.Fprintf(&b, "|%d>%s/%s", sp.Port, sp.TargetPort.StrVal, sp.Name)
	}
	return b.String()
}

// namedTargetPorts fills in, in m, the pod ports of the Service's named
// targetPorts, and returns the ones that do not resolve
// (resolveNamedTargetPorts), reading the Service's EndpointSlices only when
// it has to.
//
// Every slice event of a concerned Service is a reconcile, and a reconcile
// judges everything: read anew each time, the slices of EVERY concerned
// Service were listed for the event of one (200 Services, 200 lists per pod
// start, on every node), and a list that failed refused every named port of
// the node, not the one Service's. So what a read said is kept per Service
// and used again while all of this holds:
//
//   - no slice event of the Service has been seen since the read began (the
//     watch's predicate marks the Service, namedTargetSlices; the mark is
//     taken BEFORE the read, so an event during it is not lost);
//   - the Service object and its named ports are the ones that were read
//     for (namedSpec);
//   - the Service has been concerned at every reconcile since (endNamedTargets
//     forgets one that was not);
//   - the read is younger than namedResync. That is the bound on an event the
//     agent never saw (a watch that broke and resumed past it is repaired by
//     the informer's own relist, which delivers the change as an event; this
//     is for whatever that does not cover): a kept answer is read again when
//     it is ten minutes old, and the reconciler asks to be run when the
//     oldest is due.
//
// A read that fails is the reconcile's error, so that it is retried. What it
// does to the Service depends on why the read was made. When something says
// the kept answer is not current (a slice event, other named ports, another
// Service object), the answer is dropped and THIS Service's names are
// refused; the others' kept answers stand. When the read was only the
// ten-minute one, the kept answer stays in use: an answer with no evidence of
// change may be used, a known-stale one must not. So the bound on a missed
// event is ten minutes plus however long the API cannot be read.
func (r *Reconciler) namedTargetPorts(ctx context.Context, svc *corev1.Service, key string, m map[uint32]uint32, namedPorts []corev1.ServicePort) (map[uint32]unresolvedTarget, error) {
	spec, now := namedSpec(svc, namedPorts), r.now()
	r.namedMu.Lock()
	kept, ok := r.resolutions[key]
	changed := r.sliceChanged[key]
	delete(r.sliceChanged, key)
	r.namedMu.Unlock()
	if ok && !changed && kept.spec == spec && now.Sub(kept.at) < namedResync {
		maps.Copy(m, kept.ports)
		for _, sp := range namedPorts {
			if u, unresolved := kept.unresolved[uint32(sp.Port)]; unresolved {
				r.warnUnresolvedOnce(ctx, key, sp, u.why)
			}
		}
		return kept.unresolved, nil
	}
	unresolved, err := r.resolveNamedTargetPorts(ctx, svc, key, m, namedPorts)
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	if err != nil {
		if ok && !changed && kept.spec == spec {
			// Nothing says the kept answer changed: it was only due to be
			// read again. It stays in use, its age with it, and the error
			// has the read retried. Dropping it here had the ten-minute
			// re-read refuse every concerned Service of the node whenever
			// the API server was away at that moment.
			maps.Copy(m, kept.ports)
			return kept.unresolved, err
		}
		// A slice event was seen, or the Service's named ports are others:
		// the kept answer is known not to be current. Refused until the
		// read succeeds.
		delete(r.resolutions, key)
		return unresolved, err
	}
	res := resolution{spec: spec, at: now, ports: map[uint32]uint32{}, unresolved: unresolved}
	for _, sp := range namedPorts {
		if pod := m[uint32(sp.Port)]; pod != 0 {
			res.ports[uint32(sp.Port)] = pod
		}
	}
	if r.resolutions == nil {
		r.resolutions = map[string]resolution{}
	}
	r.resolutions[key] = res
	return unresolved, nil
}

// namedResyncDue returns how long until the oldest kept resolution is to be
// read again, zero when none is kept.
func (r *Reconciler) namedResyncDue() time.Duration {
	now := r.now()
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	var due time.Duration
	for _, res := range r.resolutions {
		due = soonest(due, max(res.at.Add(namedResync).Sub(now), time.Second))
	}
	return due
}

// setNamedTargets replaces the set (tests).
func (r *Reconciler) setNamedTargets(keys map[string]struct{}) {
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	r.namedTargets = keys
}

// warnUnresolvedOnce logs an unresolved named targetPort when it was not
// already unresolved, for the same reason, at the reconcile before: once per
// Service, port and reason, not once per reconcile.
func (r *Reconciler) warnUnresolvedOnce(ctx context.Context, key string, sp corev1.ServicePort, why string) {
	id := fmt.Sprintf("%s:%d %s", key, sp.Port, why)
	r.namedMu.Lock()
	_, said := r.warned[id]
	if r.warnedNow == nil {
		r.warnedNow = map[string]struct{}{}
	}
	r.warnedNow[id] = struct{}{}
	r.namedMu.Unlock()
	if said {
		return
	}
	r.Log.WarnContext(ctx, "a Service port has a named targetPort that does not resolve to a pod port; the port is refused until it does (the mesh service behind it cannot be addressed on that port, a backendRef to it is answered 500, and a rule for it is attached to nothing)",
		"service", key, "port", sp.Port, "targetPort", sp.TargetPort.StrVal, "reason", why)
}

// namedTargetSlices passes the events of the EndpointSlices of those Services.
// A named targetPort resolves, or stops resolving, when a slice of its
// Service appears, changes its ports or goes; nothing else the reconciler
// watches changes then, and the port stays refused (or resolved to a number
// the pods have left) until it is told. The watch is metadata-only: the
// slice's Service is in its labels, and the slices themselves are read from
// the API for the few Services concerned.
//
// An update is judged by BOTH of its objects. A slice leaves a Service by a
// change of its label (moved to another Service, or the label removed), and
// that update's new object no longer names the Service it left: judged by the
// new object alone, as predicate.NewPredicateFuncs does, the Service that lost
// its slice was never marked, and its kept answer was used for ten minutes
// more (still delivered after its last slice was gone; still refused after
// the slice that disagreed had left).
func (r *Reconciler) namedTargetSlices() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return r.sliceOfANamedTarget(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return r.sliceOfANamedTarget(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return r.sliceOfANamedTarget(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			was, is := r.sliceOfANamedTarget(e.ObjectOld), r.sliceOfANamedTarget(e.ObjectNew)
			return was || is
		},
	}
}

// sliceOfANamedTarget reports whether o is an EndpointSlice of a concerned
// Service, and marks that Service: the reconcile this event causes reads its
// slices again, and no other's (namedTargetPorts).
func (r *Reconciler) sliceOfANamedTarget(o client.Object) bool {
	if o == nil {
		return false
	}
	svc := o.GetLabels()[discoveryv1.LabelServiceName]
	if svc == "" {
		return false
	}
	key := serviceref.New(o.GetNamespace(), svc).Key()
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	_, concerned := r.namedTargets[key]
	if concerned {
		if r.sliceChanged == nil {
			r.sliceChanged = map[string]bool{}
		}
		r.sliceChanged[key] = true
	}
	return concerned
}

// servicePortMapAndFacts returns the port map of one Service when it is a
// user's (user), and records in facts, for a Service a route names (routed),
// what it says of its ports.
//
// err is a failed read of the EndpointSlices of a Service with a named
// targetPort: the port is refused meanwhile, and the reconcile must run again.
//
// A named targetPort is resolved for every Service the node's tables can
// reach a mesh service THROUGH, not only for one a route names: a Service a
// route names, one a service-wide filter targets, and one that fronts a mesh
// service this node holds rows of (concerned). The port map of every user
// Service is handed to the cache, and an unresolved name in it is refused;
// while only the routed ones were resolved, a mesh service behind an unrouted
// Service with `targetPort: <name>`, the common shape, had no authority at
// all on the proxy port and the loopback listener (its Service port was
// "unresolved" for ever, its pod port is not a port of the Service), with
// nothing logged and no route whose status could say so. A Service that
// fronts nothing of the mesh on this node is not read: its port map is used
// by no table.
func (r *Reconciler) servicePortMapAndFacts(ctx context.Context, svc *corev1.Service, key string, routed bool, also map[string]struct{}, facts map[string]portFacts) (m map[uint32]uint32, user bool, err error) {
	if routed {
		facts[key] = servicePortFacts(svc)
	}
	if svc.Labels[aetherlabels.LabelMeshService] == "true" {
		if routed && facts[key].absent {
			// A generated Service with no real port says nothing: the
			// pods' own ports are all there is, for the tables and here.
			facts[key] = r.withPods(ctx, key, facts[key], nil)
		}
		return nil, false, nil
	}
	m, namedPorts := servicePortMap(svc)
	var unresolved map[uint32]unresolvedTarget
	if len(namedPorts) > 0 && r.concerned(ctx, key, routed, also) {
		// Concerned BEFORE its slices are read: a slice that appears while
		// this reconcile is still reading the next Service's must reconcile
		// again, or the name stays unresolved with nothing left to say so.
		r.addNamedTarget(key)
		unresolved, err = r.namedTargetPorts(ctx, svc, key, m, namedPorts)
	}
	if routed {
		f := facts[key]
		f.unresolved = unresolved
		facts[key] = r.withPods(ctx, key, f, m)
	}
	return m, true, err
}

// concerned reports whether the node's tables can reach a mesh service
// through the user's Service key: a route names it, a service-wide filter
// targets it (also), or this node holds rows of the mesh service of that
// name.
func (r *Reconciler) concerned(ctx context.Context, key string, routed bool, also map[string]struct{}) bool {
	if _, filtered := also[key]; routed || filtered {
		return true
	}
	if r.PodPorts == nil {
		return false
	}
	_, held := r.PodPorts.PodPortClasses(ctx, key)
	return held
}

// withPods returns the facts of a service only its pods can speak for (a
// user's Service, or a service with no Service object) with what is needed to
// judge its ports: the Service's port map (Service port to pod port, 0 for a
// named targetPort that was not resolved), and what this node's tables serve
// the pod ports as. The judging is per route: judgedFacts.
func (r *Reconciler) withPods(ctx context.Context, key string, f portFacts, portMap map[uint32]uint32) portFacts {
	f.pods = make(map[uint32]uint32, len(portMap))
	for port, pod := range portMap {
		if pod != 0 {
			f.pods[port] = pod
		}
	}
	if r.PodPorts != nil {
		f.classes, f.held = r.PodPorts.PodPortClasses(ctx, key)
	}
	return f
}

// unresolvedTarget is a Service port whose targetPort is a NAME that does not
// resolve to a pod port: the name, and why.
type unresolvedTarget struct {
	name string
	why  string
}

// resolveNamedTargetPorts fills in, in m, the pod port of every Service port
// whose targetPort is a NAME, for a Service a route names, and returns the
// ports it could not resolve. The registry carries no port names; the
// cluster's own answer is in the Service's EndpointSlices, whose ports carry
// the Service port's name and the number the name resolved to on the pods
// behind it.
//
// A port that does not resolve stays 0 in m, and that is a refusal (owner's
// ruling of 2026-10-13): the data plane answers 500 for a backendRef to it
// and attaches a rule for it to nothing, and the status says False with the
// reason returned here. It used to mean "the port the pods declare". There
// are three reasons: no EndpointSlice of the Service names the port yet (a
// Service created before its pods), the slices resolve the name to different
// numbers, and the slices could not be read. The last is also the error
// returned, which fails the reconcile so that it is run again: an older
// resolution is not kept, because nothing shows it is still true.
//
// The slices are read from the API, uncached and only for these Services: an
// EndpointSlice informer would hold every slice of the cluster in every node
// agent for the sake of the few Services that are both route targets and use
// a named targetPort. What the reconciler watches of them is their metadata
// only (namedTargetSlices), which is what tells it to read again.
func (r *Reconciler) resolveNamedTargetPorts(ctx context.Context, svc *corev1.Service, key string, m map[uint32]uint32, namedPorts []corev1.ServicePort) (map[uint32]unresolvedTarget, error) {
	resolved, conflicts := map[string]uint32{}, map[string][]uint32{}
	var readErr error
	// No reader (never so in the agent, which is given the manager's): the
	// names stay unresolved and refused, and there is nothing to retry.
	if r.APIReader != nil {
		var slices discoveryv1.EndpointSliceList
		readErr = r.APIReader.List(ctx, &slices, client.InNamespace(svc.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name})
		if readErr == nil {
			resolved, conflicts = namedPortNumbers(slices.Items)
		}
	}
	var unresolved map[uint32]unresolvedTarget
	for _, sp := range namedPorts {
		if n := resolved[sp.Name]; n != 0 {
			m[uint32(sp.Port)] = n
			continue
		}
		why := "no EndpointSlice of the Service resolves yet"
		switch {
		case readErr != nil:
			why = "could not be read from the Service's EndpointSlices"
		case len(conflicts[sp.Name]) > 0:
			why = "the Service's EndpointSlices resolve to different ports (" + joinPorts(conflicts[sp.Name]) + ")"
		}
		if unresolved == nil {
			unresolved = map[uint32]unresolvedTarget{}
		}
		unresolved[uint32(sp.Port)] = unresolvedTarget{name: sp.TargetPort.StrVal, why: why}
		r.warnUnresolvedOnce(ctx, key, sp, why)
	}
	if readErr != nil {
		return unresolved, fmt.Errorf("read the EndpointSlices of Service %s, which has a named targetPort: %w", key, readErr)
	}
	return unresolved, nil
}

func joinPorts(ports []uint32) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.FormatUint(uint64(p), 10))
	}
	return strings.Join(parts, ", ")
}

// namedPortNumbers returns, per Service port name, the number the
// EndpointSlices give it, and, for a name the slices disagree about, nothing
// in the first map and the numbers they give, sorted, in the second.
func namedPortNumbers(slices_ []discoveryv1.EndpointSlice) (resolved map[string]uint32, conflicts map[string][]uint32) {
	seen := map[string][]uint32{}
	for _, sl := range slices_ {
		for _, p := range sl.Ports {
			if p.Name == nil || p.Port == nil || (p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP) {
				continue
			}
			if n := uint32(*p.Port); !slices.Contains(seen[*p.Name], n) {
				seen[*p.Name] = append(seen[*p.Name], n)
			}
		}
	}
	resolved, conflicts = map[string]uint32{}, map[string][]uint32{}
	for name, numbers := range seen {
		if len(numbers) == 1 {
			resolved[name] = numbers[0]
			continue
		}
		slices.Sort(numbers)
		conflicts[name] = numbers
	}
	return resolved, conflicts
}

// routeServiceKeys returns, sorted, the key of every Service the routes name:
// each parent and each backend.
func routeServiceKeys(routes map[string][]proxy.GammaRoute) []string {
	keys := map[string]struct{}{}
	for parent, rules := range routes {
		keys[parent] = struct{}{}
		for _, rule := range rules {
			for _, b := range rule.Backends {
				keys[b.Service] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// servicePortMap returns a Service's TCP port map (Service port -> pod port,
// 0 for a named targetPort) and the ports whose targetPort is a name.
func servicePortMap(svc *corev1.Service) (map[uint32]uint32, []corev1.ServicePort) {
	m := make(map[uint32]uint32, len(svc.Spec.Ports))
	var named []corev1.ServicePort
	for _, sp := range svc.Spec.Ports {
		if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
			continue
		}
		port := uint32(sp.Port)
		switch {
		case sp.TargetPort.Type == intstr.String:
			m[port] = 0
			named = append(named, sp)
		case sp.TargetPort.IntVal > 0:
			m[port] = uint32(sp.TargetPort.IntVal)
		default:
			m[port] = port
		}
	}
	return m, named
}

// addToPortSets records port in portSets[key], skipping zero ports.
func addToPortSets(portSets map[string]map[uint32]struct{}, key string, port uint32) {
	if port == 0 {
		return
	}
	if portSets[key] == nil {
		portSets[key] = map[uint32]struct{}{}
	}
	portSets[key][port] = struct{}{}
}

// buildRouteTargetPorts converts portSets (keyed by service) into sorted port slices.
func buildRouteTargetPorts(portSets map[string]map[uint32]struct{}) map[string][]uint32 {
	routeTargetPorts := make(map[string][]uint32, len(portSets))
	for key, set := range portSets {
		ports := make([]uint32, 0, len(set))
		for p := range set {
			ports = append(ports, p)
		}
		slices.Sort(ports) // stable order so the cache's change-detection is deterministic
		routeTargetPorts[key] = ports
	}
	return routeTargetPorts
}

// writeHTTPRouteStatuses publishes Gateway API status for every Service-parented
// HTTPRoute. It returns how long until a status it held back is due (zero:
// none), and the writes that failed.
func (r *Reconciler) writeHTTPRouteStatuses(ctx context.Context, httpList *gatewayv1.HTTPRouteList, grants []gatewayv1beta1.ReferenceGrant, facts map[string]portFacts, seen map[string]struct{}) (time.Duration, error) {
	var wait time.Duration
	var errs []error
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		in := routeStatusInputs{kind: "HTTPRoute", parentRefs: hr.Spec.ParentRefs, backendRefs: httpBackendRefs(hr.Spec.Rules), grants: grants, facts: facts}
		w, err := r.writeRouteStatus(ctx, hr, &hr.Status.RouteStatus, in, seen)
		wait = soonest(wait, w)
		errs = append(errs, r.statusWriteError(ctx, in.kind, hr, err))
	}
	return wait, errors.Join(errs...)
}

// writeGRPCRouteStatuses is writeHTTPRouteStatuses for GRPCRoutes.
func (r *Reconciler) writeGRPCRouteStatuses(ctx context.Context, grpcList *gatewayv1.GRPCRouteList, grants []gatewayv1beta1.ReferenceGrant, facts map[string]portFacts, seen map[string]struct{}) (time.Duration, error) {
	var wait time.Duration
	var errs []error
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		in := routeStatusInputs{kind: "GRPCRoute", parentRefs: gr.Spec.ParentRefs, backendRefs: grpcBackendRefs(gr.Spec.Rules), grants: grants, facts: facts}
		w, err := r.writeRouteStatus(ctx, gr, &gr.Status.RouteStatus, in, seen)
		wait = soonest(wait, w)
		errs = append(errs, r.statusWriteError(ctx, in.kind, gr, err))
	}
	return wait, errors.Join(errs...)
}

// statusWriteError logs a failed status write and returns it as the
// reconcile's error, except a conflict (and a route that is gone): the route
// changed under the write, which is an event the reconciler is run for.
func (r *Reconciler) statusWriteError(ctx context.Context, kind string, obj client.Object, err error) error {
	if err == nil {
		return nil
	}
	r.Log.WarnContext(ctx, "failed to write route status", "kind", kind, "route", obj.GetName(), "namespace", obj.GetNamespace(), "error", err.Error())
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("write the status of %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
}

// routeStatusInputs is what one route's status is computed from.
type routeStatusInputs struct {
	kind        string
	parentRefs  []gatewayv1.ParentReference
	backendRefs []gatewayv1.BackendObjectReference
	grants      []gatewayv1beta1.ReferenceGrant
	// facts: every named Service's ports, as this node knows them.
	facts map[string]portFacts
}

// writeRouteStatus upserts our (mesh controller) RouteParentStatus for each
// Service parentRef of the route, setting Accepted (True when we attached it;
// False with NoMatchingParent when the parentRef names a port that is not an
// HTTP port of the Service, in which case the route is attached to nothing)
// and ResolvedRefs per the backend check. It preserves status entries owned by
// other controllers and only issues a status update when something changed.
//
// Every condition is computed here, now, from the route, the grants, the
// Services and the pod-port verdicts (judged.go): nothing of what the status
// said before is kept, except those verdicts, for the ports this node cannot
// judge.
func (r *Reconciler) writeRouteStatus(ctx context.Context, obj client.Object, status *gatewayv1.RouteStatus, in routeStatusInputs, seen map[string]struct{}) (time.Duration, error) {
	generation := obj.GetGeneration()
	parents := status.Parents
	facts, record := judgedFacts(in.facts, priorJudgements(parents, generation),
		routePortRefs(obj.GetNamespace(), in.parentRefs, in.backendRefs))
	ok, reason, msg := resolveRefs(facts, obj.GetNamespace(), in.kind, in.backendRefs, in.grants)
	resolved := resolvedCondition(ok, reason, msg)

	changed := false
	var written []gatewayv1.ParentReference
	want := newWanted(generation)
	for _, p := range in.parentRefs {
		if p.Group != nil && string(*p.Group) != "" {
			continue
		}
		if p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		written = append(written, p)
		conditions := []gatewaystatus.Condition{acceptedCondition(p, facts, obj.GetNamespace()), resolved}
		var upd, removed bool
		if len(record) > 0 {
			conditions = append(conditions, judgedCondition(record))
		} else {
			parents, removed = gatewaystatus.RemoveRouteParentCondition(parents, gatewaystatus.MeshControllerName, p, JudgedConditionType)
		}
		want.add(p, conditions)
		parents, upd = gatewaystatus.MergeRouteParentStatus(parents, gatewaystatus.MeshControllerName, p, generation, conditions...)
		changed = changed || upd || removed
	}
	// The mesh's entry of a parentRef the route no longer names (an edited
	// port, a removed parent) goes with it.
	parents, pruned := gatewaystatus.PruneRouteParentStatus(parents, gatewaystatus.MeshControllerName, written)
	key := contestKey(in.kind, obj)
	seen[key] = struct{}{}
	now := r.now()
	if !changed && !pruned {
		r.contested.stands(key, want.String(), now)
		return 0, nil
	}
	if wait := r.contested.holdBack(key, want.String(), now); wait > 0 {
		return wait, nil
	}
	status.Parents = parents
	if err := r.Status().Update(ctx, obj); err != nil {
		return 0, err
	}
	r.contested.wrote(key, want.String(), now)
	return 0, nil
}

// acceptedCondition is the Accepted condition of one Service parentRef.
func acceptedCondition(p gatewayv1.ParentReference, facts map[string]portFacts, routeNamespace string) gatewaystatus.Condition {
	accepted := gatewaystatus.Condition{
		Type:    string(gatewayv1.RouteConditionAccepted),
		Status:  metav1.ConditionTrue,
		Reason:  string(gatewayv1.RouteReasonAccepted),
		Message: "Route accepted by the aether mesh",
	}
	if problem := parentPortProblem(facts, p, routeNamespace); problem != "" {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = string(gatewayv1.RouteReasonNoMatchingParent)
		accepted.Message = problem
	}
	return accepted
}

// resolvedCondition is the ResolvedRefs condition of a route, as written on
// each of its parents.
func resolvedCondition(ok bool, reason, msg string) gatewaystatus.Condition {
	resolved := gatewaystatus.Condition{
		Type:    string(gatewayv1.RouteConditionResolvedRefs),
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: msg,
	}
	if !ok {
		resolved.Status = metav1.ConditionFalse
	}
	return resolved
}

func httpBackendRefs(rules []gatewayv1.HTTPRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

func grpcBackendRefs(rules []gatewayv1.GRPCRouteRule) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	for _, rule := range rules {
		for _, b := range rule.BackendRefs {
			refs = append(refs, b.BackendObjectReference)
		}
	}
	return refs
}

// buildGammaRoute projects an HTTPRoute rule via the shared gammaproject projector and
// converts it to the agent's in-memory GammaRoute. The projector is shared with the
// registrar's cross-cluster export controller (proposal 026 EM1c) so a cluster's local
// routing and the config it exports for peers never drift.
func (r *Reconciler) buildGammaRoute(rule gatewayv1.HTTPRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter, serviceFilters []*registryv1.ExtensionFilter) proxy.GammaRoute {
	return proxy.GammaRouteFromProto(gammaproject.ProjectHTTPRule(rule, routeNamespace, routeKind, r.MeshDomain, grants, httpFilterSpecs(httpFilters), serviceFilters))
}

// buildGammaRouteFromGRPC projects a GRPCRoute rule via the shared projector.
func (r *Reconciler) buildGammaRouteFromGRPC(rule gatewayv1.GRPCRouteRule, routeNamespace, routeKind string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configapisv1.HTTPFilter, serviceFilters []*registryv1.ExtensionFilter) proxy.GammaRoute {
	return proxy.GammaRouteFromProto(gammaproject.ProjectGRPCRule(rule, routeNamespace, routeKind, r.MeshDomain, grants, httpFilterSpecs(httpFilters), serviceFilters))
}

// httpFilterSpecs extracts each K8s HTTPFilter's proto spec for the shared projector
// (which is K8s-type-agnostic). Returns nil when there are none.
func httpFilterSpecs(in map[string]*configapisv1.HTTPFilter) map[string]*configprotov1.HTTPFilterSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*configprotov1.HTTPFilterSpec, len(in))
	for k, hf := range in {
		out[k] = hf.Spec
	}
	return out
}
