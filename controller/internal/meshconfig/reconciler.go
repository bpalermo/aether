package meshconfig

import (
	"context"
	"fmt"
	"log/slog"

	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciler projects the singleton MeshConfig CR into a ConfigMap that the agent
// mounts and loads. It re-validates with protovalidate before writing, so an
// invalid CR that slipped past the (best-effort) webhook never overwrites the
// last-good ConfigMap — the failure surfaces on the CR's status instead.
type Reconciler struct {
	client.Client
	ConfigMapName      string
	ConfigMapNamespace string
	Log                *slog.Logger
}

// SetupWithManager registers the reconciler to watch the MeshConfig CR.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.MeshConfig{}).
		Named("meshconfig").
		Complete(r)
}

// Reconcile validates the singleton MeshConfig and upserts the projected
// ConfigMap. A validation failure is recorded on the CR status and does not
// return an error (retrying wouldn't help an invalid spec).
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Name != SingletonName {
		// Only the singleton is authoritative; ignore any other names.
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

	if err := Validate(mc.Spec); err != nil {
		r.Log.ErrorContext(ctx, "MeshConfig is invalid; keeping last-good ConfigMap",
			"name", req.Name, "error", err)
		r.setProjected(ctx, mc, false, err.Error())
		return reconcile.Result{}, nil
	}

	data, err := RenderConfigMapData(mc.Spec)
	if err != nil {
		return reconcile.Result{}, err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.ConfigMapName,
			Namespace: r.ConfigMapNamespace,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = data
		return nil
	})
	if err != nil {
		r.setProjected(ctx, mc, false, err.Error())
		return reconcile.Result{}, fmt.Errorf("project MeshConfig ConfigMap: %w", err)
	}

	r.Log.InfoContext(ctx, "projected MeshConfig into ConfigMap",
		"configMap", r.ConfigMapNamespace+"/"+r.ConfigMapName, "operation", op)
	r.setProjected(ctx, mc, true, "projected to "+r.ConfigMapName)
	return reconcile.Result{}, nil
}

// setProjected records the "Projected" status condition. Best-effort: a status
// write failure is logged, not surfaced.
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
	mc.Status.Conditions = []metav1.Condition{cond}
	if err := r.Status().Update(ctx, mc); err != nil {
		r.Log.WarnContext(ctx, "failed to update MeshConfig status", "error", err)
	}
}
