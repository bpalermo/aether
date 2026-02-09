package controller

import (
	"context"

	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/registrar/internal/registry"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type RegistrarController struct {
	l logr.Logger

	reg registry.Registry
}

func NewRegistrarController(registry registry.Registry, l logr.Logger) *RegistrarController {
	return &RegistrarController{
		l:   l,
		reg: registry,
	}
}

func (rc *RegistrarController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return false
			}
			// Only reconcile pods with a specific label
			_, hasLabel := pod.Labels[constants.AetherServiceLabel]
			return hasLabel
		})).
		Complete(rc)
}

func (rc *RegistrarController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	return ctrl.Result{}, nil
}
