// Package endpointpolicy contains the node agent's EndpointPolicy controller: it
// watches the service-scoped delivery policies (proposal 034 Phase 1b) and projects
// them into the snapshot cache as a "<ns>/<svc>" → "<volume>/<file>" map.
//
// The pod annotation endpoint.aether.io/uds-socket wins over a policy; the cache
// applies that precedence when it resolves a pod's delivery address.
package endpointpolicy

import (
	"context"
	"log/slog"
	"sort"

	configapisv1 "aethermesh.dev/common/apis/config/v1"
	"aethermesh.dev/common/crdcheck"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/serviceref"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// PolicySink receives the projected per-service delivery policies (the snapshot cache).
type PolicySink interface {
	// SetUDSServicePolicies replaces the service-scoped UDS delivery map, keyed by
	// the namespace-qualified "<ns>/<svc>" serviceref key (020 Part 1); values are
	// the declared "<volume>/<file>" socket.
	SetUDSServicePolicies(policies map[string]string)
}

// Reconciler watches EndpointPolicies cluster-wide and projects, on any change,
// the complete service→socket map into the sink. Level-based: each reconcile
// re-lists, so adds/updates/deletes converge without delta tracking.
type Reconciler struct {
	client.Client

	Sink PolicySink
	Log  *slog.Logger

	// enabled is set in SetupWithManager when the EndpointPolicy CRD is served.
	// Watching (or listing) a type whose CRD is absent wedges the manager on cache
	// sync / errors every reconcile, so an un-installed CRD leaves the feature
	// inert with a warning (proposal 031). Detection is setup-time: installing the
	// CRD later needs an agent restart.
	enabled bool
}

// SetupWithManager registers the reconciler to watch EndpointPolicies, unless the
// CRD is absent — in which case it registers nothing and the pod annotation stays
// the only way to request UDS delivery.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log = commonlog.Named(r.Log, "endpointpolicy")

	present, err := crdcheck.Present(mgr.GetRESTMapper(), configapisv1.GroupVersion.WithKind(configapisv1.EndpointPolicyKind))
	if err != nil {
		return err
	}
	r.enabled = present
	if !present {
		r.Log.Warn("EndpointPolicy CRD not present; service-scoped UDS delivery disabled until it is installed and the agent restarts")
		return nil
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&configapisv1.EndpointPolicy{}).
		Named("endpointpolicy").
		Complete(r)
}

// Reconcile re-lists every EndpointPolicy and replaces the sink's per-service
// delivery map.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	if !r.enabled {
		return reconcile.Result{}, nil
	}
	list := &configapisv1.EndpointPolicyList{}
	if err := r.List(ctx, list); err != nil {
		return reconcile.Result{}, err
	}
	policies := r.project(ctx, list.Items)
	r.Sink.SetUDSServicePolicies(policies)
	r.Log.DebugContext(ctx, "projected endpoint policies", "policies", len(list.Items), "services", len(policies))
	return reconcile.Result{}, nil
}

// project builds the "<ns>/<svc>" → socket map. Attachment is same-namespace and
// core-group/Service only, matching the admission webhook and the Service-attached
// filters (025 M3).
//
// At most one policy per service: two policies naming one Service are a config
// error, and the lexicographically smallest policy name wins so the projection is
// a pure function of the object set — a value that flipped with map iteration
// order would churn the snapshot on every reconcile.
func (r *Reconciler) project(ctx context.Context, items []configapisv1.EndpointPolicy) map[string]string {
	byName := make([]string, 0, len(items))
	specs := make(map[string]*configapisv1.EndpointPolicy, len(items))
	for i := range items {
		key := serviceref.New(items[i].GetNamespace(), items[i].GetName()).Key()
		byName = append(byName, key)
		specs[key] = &items[i]
	}
	sort.Strings(byName)

	policies := make(map[string]string, len(items))
	winner := make(map[string]string, len(items))
	for _, policyKey := range byName {
		ep := specs[policyKey]
		serviceKey, socket, ok := serviceTarget(ep)
		if !ok {
			continue
		}
		if prev, dup := winner[serviceKey]; dup {
			r.Log.WarnContext(ctx, "ignoring EndpointPolicy: the service already has one",
				"policy", policyKey, "service", serviceKey, "inEffect", prev)
			continue
		}
		winner[serviceKey] = policyKey
		policies[serviceKey] = socket
	}
	return policies
}

// serviceTarget returns the mesh service key and socket an EndpointPolicy declares,
// or ok=false when it targets something aether does not attach to. The target name
// is resolved in the policy's own namespace.
func serviceTarget(ep *configapisv1.EndpointPolicy) (serviceKey, socket string, ok bool) {
	spec := ep.Spec
	if spec == nil || spec.GetUdsSocket() == "" {
		return "", "", false
	}
	target := spec.GetTargetRef()
	if target == nil || target.GetName() == "" || target.GetKind() != "Service" {
		return "", "", false
	}
	if g := target.GetGroup(); g != "" && g != "core" {
		return "", "", false
	}
	return serviceref.New(ep.GetNamespace(), target.GetName()).Key(), spec.GetUdsSocket(), true
}
