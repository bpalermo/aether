package meshconfig

import (
	"context"
	"fmt"
	"log/slog"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fieldOwner is the Server-Side Apply field manager for everything the controller
// writes (the projected ConfigMap and the MeshConfig status).
const fieldOwner = "aether-controller"

// Reconciler projects each namespace's MeshConfig CR into a ConfigMap in that SAME
// namespace (co-located), which the agent/edge in that namespace mount. MeshConfig
// is namespaced: a namespace's `default` CR overrides the FallbackNamespace
// (aether-system) MeshConfig field-by-field, so a namespace inherits the mesh-wide
// config unless it sets its own. It re-validates with protovalidate before writing,
// so an invalid CR that slipped past the (best-effort) webhook never overwrites the
// last-good ConfigMap — the failure surfaces on the CR's status instead.
type Reconciler struct {
	client.Client
	ConfigMapName     string
	FallbackNamespace string
	Log               *slog.Logger
}

// SetupWithManager registers the reconciler. A change to a namespace's MeshConfig
// re-projects that namespace (For); a change to the FallbackNamespace MeshConfig
// re-projects every inheriting namespace (the fan-out map func).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.MeshConfig{}).
		Watches(&crdv1.MeshConfig{}, handler.EnqueueRequestsFromMapFunc(r.enqueueInheritors)).
		Named("meshconfig").
		Complete(r)
}

// enqueueInheritors fans a FallbackNamespace MeshConfig change out to every other
// namespace's MeshConfig (which inherit from it). Changes outside the fallback
// namespace enqueue nothing here — For() already re-projects them.
func (r *Reconciler) enqueueInheritors(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.FallbackNamespace {
		return nil
	}
	var list crdv1.MeshConfigList
	if err := r.List(ctx, &list); err != nil {
		r.Log.WarnContext(ctx, "failed to list MeshConfigs for fallback fan-out", "error", err)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]
		if m.Namespace == r.FallbackNamespace || m.Name != SingletonName {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	}
	return reqs
}

// Reconcile validates a namespace's effective MeshConfig and upserts the co-located
// ConfigMap. A validation failure is recorded on the CR status and does not return
// an error (retrying wouldn't help an invalid spec).
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Name != SingletonName {
		// Only the per-namespace singleton is authoritative; ignore any other names.
		return reconcile.Result{}, nil
	}

	mc := &crdv1.MeshConfig{}
	if err := r.Get(ctx, req.NamespacedName, mc); err != nil {
		if apierrors.IsNotFound(err) {
			// CR deleted: leave the last-good ConfigMap in place so already-running
			// pods keep their config. Recreating the CR re-projects.
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	spec, err := r.effectiveSpec(ctx, mc)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := Validate(spec); err != nil {
		r.Log.ErrorContext(ctx, "MeshConfig is invalid; keeping last-good ConfigMap",
			"namespace", req.Namespace, "error", err)
		r.setProjected(ctx, mc, false, err.Error())
		return reconcile.Result{}, nil
	}

	data, err := RenderConfigMapData(spec)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Server-Side Apply: declare the desired ConfigMap and let the apiserver merge.
	// The controller owns the `data` field (force ownership), so it converges the
	// projection without a read-modify-write and without fighting other managers.
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: r.ConfigMapName, Namespace: req.Namespace},
		Data:       data,
	}
	if err := r.serverApply(ctx, cm); err != nil {
		r.setProjected(ctx, mc, false, err.Error())
		return reconcile.Result{}, fmt.Errorf("apply MeshConfig ConfigMap: %w", err)
	}

	target := req.Namespace + "/" + r.ConfigMapName
	r.Log.InfoContext(ctx, "projected MeshConfig into ConfigMap", "configMap", target)
	r.setProjected(ctx, mc, true, "projected to "+target)
	return reconcile.Result{}, nil
}

// effectiveSpec returns the config to project for mc's namespace: the
// FallbackNamespace MeshConfig as the base with mc's spec merged over it (mc's set
// fields win, edition-2024 presence respected). The fallback namespace itself — or a
// missing/empty fallback — projects mc's spec verbatim.
func (r *Reconciler) effectiveSpec(ctx context.Context, mc *crdv1.MeshConfig) (*configv1.MeshConfigSpec, error) {
	if mc.Namespace == r.FallbackNamespace {
		return mc.Spec, nil
	}
	base := &crdv1.MeshConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.FallbackNamespace, Name: SingletonName}, base); err != nil {
		if apierrors.IsNotFound(err) {
			return mc.Spec, nil // no fallback source; project this namespace's own spec
		}
		return nil, fmt.Errorf("get fallback MeshConfig: %w", err)
	}
	if base.Spec == nil {
		return mc.Spec, nil
	}
	eff := proto.Clone(base.Spec).(*configv1.MeshConfigSpec)
	if mc.Spec != nil {
		proto.Merge(eff, mc.Spec)
	}
	return eff, nil
}

// serverApply server-side-applies an object owned by the controller's field
// manager (force ownership of the fields it sets).
func (r *Reconciler) serverApply(ctx context.Context, obj client.Object) error {
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

// setProjected records the "Projected" status condition via Server-Side Apply on
// the status subresource. The apply object carries only TypeMeta, the name+namespace
// and status (no spec), so the controller's field manager owns just the status.
// Best-effort: a write failure is logged, not surfaced.
func (r *Reconciler) setProjected(ctx context.Context, mc *crdv1.MeshConfig, ok bool, msg string) {
	cond := metav1.Condition{
		Type:               "Projected",
		ObservedGeneration: mc.GetGeneration(),
		LastTransitionTime: metav1.Now(),
		Status:             metav1.ConditionTrue,
		Reason:             "Projected",
		Message:            msg,
	}
	if !ok {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "InvalidConfig"
	}
	apply := &crdv1.MeshConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: crdv1.GroupVersion.String(), Kind: crdv1.MeshConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: mc.GetName(), Namespace: mc.GetNamespace()},
		Status:     crdv1.MeshConfigStatus{Conditions: []metav1.Condition{cond}},
	}
	if err := r.Status().Patch(ctx, apply, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		r.Log.WarnContext(ctx, "failed to apply MeshConfig status", "error", err)
	}
}
