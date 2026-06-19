// Package edge contains the edge proxy's EdgeRoute controller: it watches
// EdgeRoute custom resources and projects them into the snapshot cache as the
// edge listener's routes (and the scoped dependency set).
package edge

import (
	"context"
	"log/slog"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RouteSink receives the full set of edge routes (the snapshot cache).
type RouteSink interface {
	SetEdgeRoutes(routes []cache.EdgeRoute)
}

// Reconciler watches EdgeRoute CRs in a namespace and pushes the complete route
// set into the cache on any change. It is level-based: each reconcile re-lists,
// so adds, updates and deletes all converge to the same projection without
// tracking deltas.
type Reconciler struct {
	client.Client

	// Sink receives the projected routes (the snapshot cache).
	Sink RouteSink
	// Namespace is the namespace EdgeRoutes are read from.
	Namespace string
	Log       *slog.Logger
}

// SetupWithManager registers the reconciler to watch EdgeRoute CRs.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "edgeroute")
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.EdgeRoute{}).
		Named("edgeroute").
		Complete(r)
}

// Reconcile re-lists every EdgeRoute in the namespace, converts them (plus the
// static seed) to cache routes, and replaces the cache's route set.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &crdv1.EdgeRouteList{}
	if err := r.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}

	routes := make([]cache.EdgeRoute, 0, len(list.Items))
	for i := range list.Items {
		spec := list.Items[i].Spec
		// A route routes nowhere without a service or without an explicit
		// external host — the edge never exposes a service at its mesh FQDN.
		if spec == nil || spec.GetService() == "" || len(spec.GetHosts()) == 0 {
			continue
		}
		routes = append(routes, cache.EdgeRoute{
			Hosts:   spec.GetHosts(),
			Service: spec.GetService(),
			Port:    spec.GetPort(),
		})
	}

	r.Log.InfoContext(ctx, "projecting edge routes", "namespace", r.Namespace, "routes", len(routes), "edgeRoutes", len(list.Items))
	r.Sink.SetEdgeRoutes(routes)
	return reconcile.Result{}, nil
}
