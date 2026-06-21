// Package edge contains the edge proxy's VirtualHost controller: it watches
// VirtualHost custom resources (and, when TLS is enabled, the Secrets they
// reference) and projects them into the snapshot cache as the edge listener's
// virtual hosts, scoped dependency set, and downstream TLS certs.
package edge

import (
	"context"
	"log/slog"
	"slices"
	"strings"

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

// RouteSink receives the projected virtual hosts and TLS certs (the snapshot cache).
type RouteSink interface {
	SetVirtualHosts(vhosts []cache.VirtualHost)
	SetEdgeTLSSecrets(ctx context.Context, certs map[string]cache.EdgeTLSCert) error
}

// Reconciler watches VirtualHost CRs in a namespace and pushes the complete
// virtual-host set into the cache on any change. It is level-based: each
// reconcile re-lists, so adds, updates and deletes all converge to the same
// projection without tracking deltas. When Secrets is non-nil it also resolves
// each virtual host's TLS cert and watches the referenced Secrets for rotation.
type Reconciler struct {
	client.Client

	// Sink receives the projected virtual hosts/certs (the snapshot cache).
	Sink RouteSink
	// Namespace is the namespace VirtualHosts (and their TLS Secrets) are read from.
	Namespace string
	// Secrets resolves VirtualHost TLS cert material; nil disables TLS resolution.
	Secrets *secret.Registry
	Log     *slog.Logger
}

// SetupWithManager registers the reconciler to watch VirtualHost CRs (and Secrets
// when TLS resolution is enabled, for hot cert rotation).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "virtualhost")
	b := ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.VirtualHost{}).
		Named("virtualhost")
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

// Reconcile re-lists every VirtualHost in the namespace, resolves each one's TLS
// cert (if any), and replaces the cache's virtual-host set + TLS certs.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &crdv1.VirtualHostList{}
	if err := r.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}

	// Deterministic keep-first on duplicate hosts (the cache drops a host already
	// claimed by an earlier vhost): order by name so the same vhost always wins.
	slices.SortFunc(list.Items, func(a, b crdv1.VirtualHost) int {
		return strings.Compare(a.Name, b.Name)
	})

	vhosts := make([]cache.VirtualHost, 0, len(list.Items))
	certs := map[string]cache.EdgeTLSCert{}
	for i := range list.Items {
		spec := list.Items[i].Spec
		// A virtual host exposes nothing without an explicit external host — the
		// edge never exposes a service at its mesh FQDN.
		if spec == nil || len(spec.GetHosts()) == 0 || len(spec.GetRoutes()) == 0 {
			continue
		}

		vh := cache.VirtualHost{Hosts: spec.GetHosts()}
		for _, hr := range spec.GetRoutes() {
			b := hr.GetBackend()
			if b == nil || b.GetService() == "" {
				continue
			}
			m := hr.GetMatch()
			vh.Routes = append(vh.Routes, cache.Route{
				Prefix:  m.GetPrefix(),
				Exact:   m.GetExact(),
				Service: b.GetService(),
				Port:    b.GetPort(),
			})
		}
		if len(vh.Routes) == 0 {
			continue
		}

		// Resolve the virtual host's downstream cert (if referenced and TLS is
		// enabled). A resolution failure leaves it without a cert (SNI miss for
		// its hosts) rather than failing the whole projection.
		if r.Secrets != nil && spec.GetTls() != nil && spec.GetTls().GetSecretName() != "" {
			provider, ref := spec.GetTls().GetProvider(), spec.GetTls().GetSecretName()
			sdsName, err := r.Secrets.SDSName(provider, ref)
			if err != nil {
				r.Log.WarnContext(ctx, "virtual host TLS provider unavailable; serving no cert for its hosts",
					"name", list.Items[i].Name, "provider", provider.String(), "error", err.Error())
			} else if cert, err := r.Secrets.Resolve(ctx, provider, ref); err != nil {
				r.Log.WarnContext(ctx, "virtual host TLS cert unresolved; serving no cert for its hosts",
					"name", list.Items[i].Name, "secret", ref, "error", err.Error())
			} else {
				vh.TLSSecret = sdsName
				certs[sdsName] = cache.EdgeTLSCert{Cert: cert.Cert, Key: cert.Key}
			}
		}
		vhosts = append(vhosts, vh)
	}

	r.Log.InfoContext(ctx, "projecting edge virtual hosts", "namespace", r.Namespace,
		"virtualHosts", len(vhosts), "crs", len(list.Items), "tlsCerts", len(certs))
	r.Sink.SetVirtualHosts(vhosts)
	if r.Secrets != nil {
		if err := r.Sink.SetEdgeTLSSecrets(ctx, certs); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}
