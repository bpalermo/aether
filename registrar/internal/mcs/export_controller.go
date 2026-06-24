// Package mcs implements Kubernetes Multi-Cluster Services (MCS-API) phase 1 for
// aether, backed by the origin-partitioned registry (proposals 018 + 006). The
// REGISTRY — not cross-cluster DNS or EndpointSlice import — is aether's
// cross-cluster plane:
//
//   - ExportController watches local ServiceExport objects and records the
//     export in the registry (registry.ServiceExporter), so other clusters can
//     import the service. It reconciles ServiceExport status (Valid/Ready).
//   - ImportGenerator reads the clusterset-wide export view back out of the
//     registry and materializes, in THIS cluster, a ServiceImport (ClusterSetIP)
//     plus a LOCAL selectorless clusterset VIP Service (a DISTINCT <svc>-clusterset
//     object with its own ClusterIP, so it coexists with the same-named per-cluster
//     mesh Service) — mirroring the registrar's mesh-Service generator. The VIP's
//     ClusterIP is stamped onto the ServiceImport's spec.IPs (the
//     <svc>.<ns>.svc.clusterset.local address). No EndpointSlice import: endpoints
//     stay in the registry; the proxy resolves cross-cluster endpoints via registry
//     EDS at dial time (a later phase — out of scope here).
//
// DNS stays STRICTLY LOCAL: each cluster materializes its own clusterset VIP from
// its own registry view. Both runnables are leader-elected (single writer) and
// require a registry backend implementing registry.ServiceExporter (etcd); they
// no-op cleanly for backends without a cross-cluster plane.
package mcs

import (
	"context"
	"log/slog"

	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// fieldOwner is the Server-Side Apply field manager for ServiceExport status.
const fieldOwner = "aether-registrar-mcs"

// ExportController reconciles ServiceExport objects into the registry's
// clusterset-wide export view: it marks the referenced mesh service exported in
// the origin-partitioned registry (this cluster's own partition) when a
// ServiceExport exists with a backing Service, and unmarks it when the
// ServiceExport is deleted. It records the standard MCS status conditions
// (Valid, Ready) on each ServiceExport.
//
// It is leader-elected via controller-runtime's manager (the manager runs
// reconcilers only on the leader), so writes to the registry partition have a
// single author per cluster.
type ExportController struct {
	client.Client
	// Exporter is the registry's cross-cluster export capability. Required.
	Exporter registry.ServiceExporter
	Log      *slog.Logger
}

// SetupWithManager registers the controller for ServiceExport objects.
func (c *ExportController) SetupWithManager(mgr ctrl.Manager) error {
	c.Log = commonlog.Named(c.Log, "mcs-export-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcsv1alpha1.ServiceExport{}).
		Named("mcs-serviceexport").
		Complete(c)
}

// Reconcile records or clears the registry export mark for one ServiceExport and
// stamps its status. The (namespace, name) of the ServiceExport IS the mesh
// service identity (MCS keys exports by the same-named Service), so the registry
// service key is the ServiceExport name.
func (c *ExportController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	se := &mcsv1alpha1.ServiceExport{}
	if err := c.Get(ctx, req.NamespacedName, se); err != nil {
		if apierrors.IsNotFound(err) {
			// ServiceExport deleted: clear the registry export mark. Other clusters'
			// ImportGenerators then drop the corresponding ServiceImport/VIP.
			if err := c.Exporter.UnsetExport(ctx, req.Name); err != nil {
				return reconcile.Result{}, err
			}
			c.Log.InfoContext(ctx, "cleared registry export for deleted ServiceExport",
				"service", req.Name, "namespace", req.Namespace)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Validate the export: the backing Service must exist (MCS pulls the service
	// config from the same-named Service). ExternalName Services are not
	// exportable per the MCS KEP.
	valid, reason, msg := c.validate(ctx, req)
	if !valid {
		c.setConditions(ctx, se, false, reason, msg, false)
		// Not exportable: ensure no stale mark lingers. A spec error won't fix on
		// retry, so don't return an error.
		if err := c.Exporter.UnsetExport(ctx, req.Name); err != nil {
			c.Log.WarnContext(ctx, "failed to clear export for invalid ServiceExport", "service", req.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	if err := c.Exporter.SetExport(ctx, req.Name, req.Namespace); err != nil {
		c.setConditions(ctx, se, true, string(mcsv1alpha1.ServiceExportReasonValid), "backing Service found",
			false)
		return reconcile.Result{}, err
	}

	c.Log.InfoContext(ctx, "recorded registry export for ServiceExport",
		"service", req.Name, "namespace", req.Namespace)
	c.setConditions(ctx, se, true, string(mcsv1alpha1.ServiceExportReasonValid), "backing Service found", true)
	return reconcile.Result{}, nil
}

// validate reports whether the ServiceExport is exportable: the same-named
// Service must exist and not be of type ExternalName.
func (c *ExportController) validate(ctx context.Context, req reconcile.Request) (ok bool, reason, msg string) {
	svc := &corev1.Service{}
	if err := c.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, string(mcsv1alpha1.ServiceExportReasonNoService), "no Service with the same name and namespace"
		}
		// Transient get error: treat as not-yet-valid without a definitive reason.
		return false, string(mcsv1alpha1.ServiceExportReasonNoService), err.Error()
	}
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return false, string(mcsv1alpha1.ServiceExportReasonInvalidServiceType), "ExternalName Services are not exportable"
	}
	return true, string(mcsv1alpha1.ServiceExportReasonValid), "backing Service found"
}

// setConditions stamps the Valid and Ready status conditions on the
// ServiceExport via Server-Side Apply (status subresource). validReason/validMsg
// describe the Valid condition; ready reflects whether the export reached the
// registry. Best-effort: a write failure is logged, not surfaced.
func (c *ExportController) setConditions(ctx context.Context, se *mcsv1alpha1.ServiceExport, valid bool, validReason, validMsg string, ready bool) {
	gen := se.GetGeneration()
	validCond := metav1.Condition{
		Type:               string(mcsv1alpha1.ServiceExportConditionValid),
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
		Status:             metav1.ConditionTrue,
		Reason:             validReason,
		Message:            validMsg,
	}
	if !valid {
		validCond.Status = metav1.ConditionFalse
	}

	readyCond := metav1.Condition{
		Type:               string(mcsv1alpha1.ServiceExportConditionReady),
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
		Status:             metav1.ConditionTrue,
		Reason:             string(mcsv1alpha1.ServiceExportReasonExported),
		Message:            "exported to the aether registry",
	}
	if !ready {
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = string(mcsv1alpha1.ServiceExportReasonPending)
		readyCond.Message = "not yet exported to the aether registry"
	}

	apply := &mcsv1alpha1.ServiceExport{
		TypeMeta:   metav1.TypeMeta{APIVersion: mcsv1alpha1.GroupVersion.String(), Kind: mcsv1alpha1.ServiceExportKindName},
		ObjectMeta: metav1.ObjectMeta{Name: se.GetName(), Namespace: se.GetNamespace()},
		Status:     mcsv1alpha1.ServiceExportStatus{Conditions: []metav1.Condition{validCond, readyCond}},
	}
	if err := c.Status().Patch(ctx, apply, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		c.Log.WarnContext(ctx, "failed to apply ServiceExport status", "service", se.GetName(), "error", err)
	}
}
