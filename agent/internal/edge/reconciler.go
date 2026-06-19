// Package edge contains the edge proxy's EdgeRoute controller: it watches
// EdgeRoute custom resources (and, when TLS is enabled, the Secrets they
// reference) and projects them into the snapshot cache as the edge listener's
// routes, scoped dependency set, and downstream TLS certs.
package edge

import (
	"context"
	"log/slog"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RouteSink receives the projected edge routes and TLS certs (the snapshot cache).
type RouteSink interface {
	SetEdgeRoutes(routes []cache.EdgeRoute)
	SetEdgeTLSSecrets(ctx context.Context, certs map[string]cache.EdgeTLSCert) error
}

// Reconciler watches EdgeRoute CRs in a namespace and pushes the complete route
// set into the cache on any change. It is level-based: each reconcile re-lists,
// so adds, updates and deletes all converge to the same projection without
// tracking deltas. When Secrets is non-nil it also resolves each route's TLS cert
// and watches the referenced Secrets for rotation.
type Reconciler struct {
	client.Client

	// Sink receives the projected routes/certs (the snapshot cache).
	Sink RouteSink
	// Namespace is the namespace EdgeRoutes (and their TLS Secrets) are read from.
	Namespace string
	// Secrets resolves EdgeRoute TLS cert material; nil disables TLS resolution.
	Secrets *secret.Registry
	Log     *slog.Logger
}

// SetupWithManager registers the reconciler to watch EdgeRoute CRs (and Secrets
// when TLS resolution is enabled, for hot cert rotation).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "edgeroute")
	b := ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.EdgeRoute{}).
		Named("edgeroute")
	if r.Secrets != nil {
		// Any Secret change in the namespace triggers a full re-list + re-resolve
		// (the kubernetes provider's rotation signal).
		b = b.Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			func(context.Context, client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "resync"}}}
			}))
	}
	return b.Complete(r)
}

// Reconcile re-lists every EdgeRoute in the namespace, resolves each route's TLS
// cert (if any), and replaces the cache's route set + TLS certs.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &crdv1.EdgeRouteList{}
	if err := r.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}

	routes := make([]cache.EdgeRoute, 0, len(list.Items))
	certs := map[string]cache.EdgeTLSCert{}
	for i := range list.Items {
		spec := list.Items[i].Spec
		// A route routes nowhere without a service or without an explicit
		// external host — the edge never exposes a service at its mesh FQDN.
		if spec == nil || spec.GetService() == "" || len(spec.GetHosts()) == 0 {
			continue
		}
		route := cache.EdgeRoute{
			Hosts:   spec.GetHosts(),
			Service: spec.GetService(),
			Port:    spec.GetPort(),
		}
		// Resolve the route's downstream cert (if referenced and TLS is enabled).
		// A resolution failure leaves the route without a cert (SNI miss for its
		// hosts) rather than failing the whole projection.
		if r.Secrets != nil && spec.GetTls() != nil && spec.GetTls().GetSecretName() != "" {
			provider, ref := spec.GetTls().GetProvider(), spec.GetTls().GetSecretName()
			sdsName, err := r.Secrets.SDSName(provider, ref)
			if err != nil {
				r.Log.WarnContext(ctx, "edge route TLS provider unavailable; serving no cert for its hosts",
					"name", list.Items[i].Name, "provider", provider.String(), "error", err.Error())
			} else if cert, err := r.Secrets.Resolve(ctx, provider, ref); err != nil {
				r.Log.WarnContext(ctx, "edge route TLS cert unresolved; serving no cert for its hosts",
					"name", list.Items[i].Name, "secret", ref, "error", err.Error())
			} else {
				route.TLSSecret = sdsName
				certs[sdsName] = cache.EdgeTLSCert{Cert: cert.Cert, Key: cert.Key}
			}
		}
		routes = append(routes, route)
	}

	r.Log.InfoContext(ctx, "projecting edge routes", "namespace", r.Namespace,
		"routes", len(routes), "edgeRoutes", len(list.Items), "tlsCerts", len(certs))
	r.Sink.SetEdgeRoutes(routes)
	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}
