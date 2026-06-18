package meshconfig

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciler projects the singleton MeshConfig CR into a ConfigMap that the
// agent DaemonSet (and any other consumer) mounts and loads. It re-validates
// with protovalidate before writing, so an invalid CR that slipped past the
// (best-effort) webhook never overwrites the last-good ConfigMap — the failure
// surfaces on the CR's status instead.
type Reconciler struct {
	client.Client
	// ConfigMapName / ConfigMapNamespace identify the projected ConfigMap.
	ConfigMapName      string
	ConfigMapNamespace string
	Log                *slog.Logger
}

// SetupWithManager registers the reconciler to watch the MeshConfig CR.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(NewUnstructured()).
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

	u := NewUnstructured()
	if err := r.Get(ctx, req.NamespacedName, u); err != nil {
		if apierrors.IsNotFound(err) {
			// CR deleted: leave the last-good ConfigMap in place so already-running
			// pods keep their config. Recreating the CR re-projects.
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	spec, _, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("read MeshConfig spec: %w", err)
	}
	cfg, validateErr := ProtoFromSpec(spec)
	if validateErr != nil {
		r.Log.ErrorContext(ctx, "MeshConfig is invalid; keeping last-good ConfigMap",
			"name", req.Name, "error", validateErr)
		r.setProjected(ctx, u, false, validateErr.Error())
		return reconcile.Result{}, nil
	}

	data, err := RenderConfigMapData(cfg)
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
		r.setProjected(ctx, u, false, err.Error())
		return reconcile.Result{}, fmt.Errorf("project MeshConfig ConfigMap: %w", err)
	}

	r.Log.InfoContext(ctx, "projected MeshConfig into ConfigMap",
		"configMap", r.ConfigMapNamespace+"/"+r.ConfigMapName, "operation", op)
	r.setProjected(ctx, u, true, "projected to "+r.ConfigMapName)
	return reconcile.Result{}, nil
}

// setProjected records a single status condition reporting the last projection
// result. Best-effort: a status write failure is logged, not surfaced.
func (r *Reconciler) setProjected(ctx context.Context, u *unstructured.Unstructured, ok bool, msg string) {
	status := metav1.ConditionTrue
	reason := "Projected"
	if !ok {
		status = metav1.ConditionFalse
		reason = "InvalidConfig"
	}
	cond := map[string]any{
		"type":               "Projected",
		"status":             string(status),
		"reason":             reason,
		"message":            msg,
		"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z07:00"),
	}
	if err := unstructured.SetNestedSlice(u.Object, []any{cond}, "status", "conditions"); err != nil {
		r.Log.WarnContext(ctx, "failed to build MeshConfig status", "error", err)
		return
	}
	if err := r.Status().Update(ctx, u); err != nil {
		r.Log.WarnContext(ctx, "failed to update MeshConfig status", "error", err)
	}
}
