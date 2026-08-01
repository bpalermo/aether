// Package configexport is the registrar's cross-cluster config EXPORT controller
// (proposal 026 EM1c). Leader-elected (one author per cluster), it watches the GAMMA
// config (HTTPRoute/GRPCRoute) + MCS ServiceExports and writes the projected config of
// each EXPORTED route-target Service into the shared registry (registry.ConfigExporter),
// for peer clusters to import. It uses the SAME common/gammaproject projector the agent
// uses for local routing, so exported config and local config never drift.
package configexport

import (
	"context"
	"log/slog"
	"time"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/gammaproject"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// Controller projects exported services' GAMMA config and writes it to the registry.
type Controller struct {
	client.Client
	// Exporter is the registry's cross-cluster config write capability. Required.
	Exporter registry.ConfigExporter
	// MeshDomain resolves backends to data-plane cluster names.
	MeshDomain string
	// Cluster is this cluster's name (the export origin; used to scope cleanup).
	Cluster string
	Log     *slog.Logger

	httpFilterEnabled bool
	metrics           *metrics
}

// SetupWithManager registers the controller. It watches HTTPRoutes/GRPCRoutes (the
// config), ServiceExports (which services propagate), and ReferenceGrants (backend
// resolution); the optional HTTPFilter watch is gated on the CRD being present.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	c.Log = commonlog.Named(c.Log, "config-export-controller")
	c.metrics = newMetrics()
	enqueueAll := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})
	gvk := configapisv1.GroupVersion.WithKind(configapisv1.HTTPFilterKind)
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if !meta.IsNoMatchError(err) {
			return err
		}
		c.Log.Warn("HTTPFilter CRD not present; exported config omits escape-hatch filters until it is installed")
	} else {
		c.httpFilterEnabled = true
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.GRPCRoute{}, enqueueAll).
		Watches(&mcsv1alpha1.ServiceExport{}, enqueueAll).
		Watches(&gatewayv1beta1.ReferenceGrant{}, enqueueAll)
	if c.httpFilterEnabled {
		b = b.Watches(&configapisv1.HTTPFilter{}, enqueueAll)
	}
	return b.Named("config-export").Complete(c)
}

// Reconcile (level-based) projects every exported route-target Service's GAMMA config
// and reconciles it into the registry: SetConfig changed/new projections, UnsetConfig
// the ones this cluster previously exported but no longer has.
func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	httpList, grpcList, grantList, exportList, err := c.listExportResources(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	exported := buildExportedSet(exportList)
	httpFilters := c.httpFilterSpecs(ctx)

	desired := c.projectHTTPRoutes(httpList, exported, grantList.Items, httpFilters)
	for svc, routes := range c.projectGRPCRoutes(grpcList, exported, grantList.Items, httpFilters) {
		desired[svc] = append(desired[svc], routes...)
	}
	desiredFilter := c.resolveChainFilters(exported, httpFilters, desired)

	current, err := c.Exporter.ListConfig(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	own := ownCurrentProjections(current, c.Cluster)

	version := time.Now().UTC().Format(time.RFC3339Nano)
	if err := c.applyDesired(ctx, desired, desiredFilter, own, version); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.pruneStale(ctx, desired, own); err != nil {
		return reconcile.Result{}, err
	}
	c.metrics.observe(ctx, len(desired))
	return reconcile.Result{}, nil
}

// listExportResources lists all resources needed for the export reconciliation.
func (c *Controller) listExportResources(ctx context.Context) (*gatewayv1.HTTPRouteList, *gatewayv1.GRPCRouteList, *gatewayv1beta1.ReferenceGrantList, *mcsv1alpha1.ServiceExportList, error) {
	httpList := &gatewayv1.HTTPRouteList{}
	if err := c.List(ctx, httpList); err != nil {
		return nil, nil, nil, nil, err
	}
	grpcList := &gatewayv1.GRPCRouteList{}
	if err := c.List(ctx, grpcList); err != nil {
		return nil, nil, nil, nil, err
	}
	grantList := &gatewayv1beta1.ReferenceGrantList{}
	if err := c.List(ctx, grantList); err != nil {
		return nil, nil, nil, nil, err
	}
	exportList := &mcsv1alpha1.ServiceExportList{}
	if err := c.List(ctx, exportList); err != nil {
		return nil, nil, nil, nil, err
	}
	return httpList, grpcList, grantList, exportList, nil
}

// buildExportedSet builds the set of exported service keys from a ServiceExportList.
func buildExportedSet(exportList *mcsv1alpha1.ServiceExportList) map[string]struct{} {
	exported := make(map[string]struct{}, len(exportList.Items))
	for i := range exportList.Items {
		se := &exportList.Items[i]
		exported[se.Namespace+"/"+se.Name] = struct{}{}
	}
	return exported
}

// projectHTTPRoutes projects exported HTTPRoute rules into GammaRoute protos.
func (c *Controller) projectHTTPRoutes(httpList *gatewayv1.HTTPRouteList, exported map[string]struct{}, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configprotov1.HTTPFilterSpec) map[string][]*registryv1.GammaRoute {
	desired := map[string][]*registryv1.GammaRoute{}
	for i := range httpList.Items {
		hr := &httpList.Items[i]
		for _, p := range gammaproject.ServiceParents(hr.Spec.ParentRefs, hr.Namespace) {
			if _, ok := exported[p.Key]; !ok {
				continue
			}
			svcFilters := gammaproject.ServiceFilters(p.Key, httpFilters) // M3 targetRef-attached
			for _, rule := range hr.Spec.Rules {
				desired[p.Key] = append(desired[p.Key], gammaproject.ProjectHTTPRule(rule, hr.Namespace, "HTTPRoute", c.MeshDomain, grants, httpFilters, svcFilters))
			}
		}
	}
	return desired
}

// projectGRPCRoutes projects exported GRPCRoute rules into GammaRoute protos.
func (c *Controller) projectGRPCRoutes(grpcList *gatewayv1.GRPCRouteList, exported map[string]struct{}, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configprotov1.HTTPFilterSpec) map[string][]*registryv1.GammaRoute {
	desired := map[string][]*registryv1.GammaRoute{}
	for i := range grpcList.Items {
		gr := &grpcList.Items[i]
		for _, p := range gammaproject.ServiceParents(gr.Spec.ParentRefs, gr.Namespace) {
			if _, ok := exported[p.Key]; !ok {
				continue
			}
			svcFilters := gammaproject.ServiceFilters(p.Key, httpFilters) // M3 targetRef-attached
			for _, rule := range gr.Spec.Rules {
				desired[p.Key] = append(desired[p.Key], gammaproject.ProjectGRPCRule(rule, gr.Namespace, "GRPCRoute", c.MeshDomain, grants, httpFilters, svcFilters))
			}
		}
	}
	return desired
}

// resolveChainFilters resolves the service-wide chain filters (025 M4) for every
// exported service, including filter-only projections (no routes). Mutates desired
// to add filter-only entries.
func (c *Controller) resolveChainFilters(exported map[string]struct{}, httpFilters map[string]*configprotov1.HTTPFilterSpec, desired map[string][]*registryv1.GammaRoute) map[string]*registryv1.ExtensionFilter {
	desiredFilter := map[string]*registryv1.ExtensionFilter{}
	for svc := range exported {
		if ef := gammaproject.ServiceChainFilter(svc, httpFilters); ef != nil {
			desiredFilter[svc] = ef
			if _, ok := desired[svc]; !ok {
				desired[svc] = nil // filter-only projection
			}
		}
	}
	return desiredFilter
}

// ownCurrentProjections filters a config projection list to those authored by cluster.
func ownCurrentProjections(current []*registryv1.ServiceConfigProjection, cluster string) map[string]*registryv1.ServiceConfigProjection {
	own := map[string]*registryv1.ServiceConfigProjection{}
	for _, p := range current {
		if p.GetOriginCluster() == cluster {
			own[p.GetService()] = p
		}
	}
	return own
}

// applyDesired writes new or changed config projections to the exporter.
func (c *Controller) applyDesired(ctx context.Context, desired map[string][]*registryv1.GammaRoute, desiredFilter map[string]*registryv1.ExtensionFilter, own map[string]*registryv1.ServiceConfigProjection, version string) error {
	for svc, routes := range desired {
		// Skip when unchanged — avoid churning importers with a fresh version every reconcile.
		if existing, ok := own[svc]; ok && routesEqual(existing.GetRoutes(), routes) && proto.Equal(existing.GetServiceFilter(), desiredFilter[svc]) {
			continue
		}
		if err := c.Exporter.SetConfig(ctx, &registryv1.ServiceConfigProjection{Service: svc, Version: version, Routes: routes, ServiceFilter: desiredFilter[svc]}); err != nil {
			c.metrics.writeError(ctx)
			c.Log.ErrorContext(ctx, "failed to export config projection", "service", svc, "error", err)
			return err
		}
		c.Log.InfoContext(ctx, "exported config projection", "service", svc, "routes", len(routes))
	}
	return nil
}

// pruneStale removes config projections this cluster authored that are no longer
// exported or configured.
func (c *Controller) pruneStale(ctx context.Context, desired map[string][]*registryv1.GammaRoute, own map[string]*registryv1.ServiceConfigProjection) error {
	for svc := range own {
		if _, ok := desired[svc]; ok {
			continue
		}
		if err := c.Exporter.UnsetConfig(ctx, svc); err != nil {
			c.metrics.writeError(ctx)
			c.Log.ErrorContext(ctx, "failed to unexport config projection", "service", svc, "error", err)
			return err
		}
		c.Log.InfoContext(ctx, "unexported config projection", "service", svc)
	}
	return nil
}

// httpFilterSpecs builds the ExtensionRef lookup (proposal 025) keyed "<ns>/<name>",
// or nil when the CRD is absent.
func (c *Controller) httpFilterSpecs(ctx context.Context) map[string]*configprotov1.HTTPFilterSpec {
	if !c.httpFilterEnabled {
		return nil
	}
	list := &configapisv1.HTTPFilterList{}
	if err := c.List(ctx, list); err != nil {
		c.Log.WarnContext(ctx, "listing HTTPFilters for export failed; omitting escape-hatch filters", "error", err)
		return nil
	}
	out := make(map[string]*configprotov1.HTTPFilterSpec, len(list.Items))
	for i := range list.Items {
		hf := &list.Items[i]
		out[hf.Namespace+"/"+hf.Name] = hf.Spec
	}
	return out
}

func routesEqual(a, b []*registryv1.GammaRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
