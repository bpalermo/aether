package controller

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/constants"
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

	clusterName string

	client client.Client
	reg    registry.Registry
}

func NewRegistrarController(clusterName string, c client.Client, registry registry.Registry, l logr.Logger) *RegistrarController {
	return &RegistrarController{
		l:           l.WithName("controller"),
		clusterName: clusterName,
		client:      c,
		reg:         registry,
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
			// Exclude kube-system and aether-system namespaces
			if pod.Namespace == "kube-system" || pod.Namespace == "aether-system" {
				return false
			}
			// Only reconcile pods with a specific label
			_, hasLabel := pod.Labels[constants.LabelAetherService]
			return hasLabel
		})).
		Complete(rc)
}

func (rc *RegistrarController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := rc.l.WithValues("pod", request.Name, "namespace", request.Namespace)

	var pod corev1.Pod
	if err := rc.client.Get(ctx, request.NamespacedName, &pod); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Pod was deleted, unregister it
			// TODO: missing handling
			log.V(1).Info("pod deleted, unregistering")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get pod")
		return ctrl.Result{}, err
	}

	serviceName, ok := pod.Labels[constants.LabelAetherService]
	if !ok {
		log.Info("pod does not have a service label, skipping")
		return ctrl.Result{}, nil
	}

	// Check if the pod is being deleted
	if !pod.DeletionTimestamp.IsZero() {
		log.V(1).Info("pod is being deleted, unregistering")
		if err := rc.reg.UnregisterEndpoints(ctx, serviceName, getPodIPs(pod)); err != nil {
			log.Error(err, "failed to unregister pod")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Only register running pods with an IP
	if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
		log.V(1).Info("registering pod", "ip", pod.Status.PodIP)

		nodeLabels, err := rc.getNodeLabels(ctx, &pod)
		if err != nil {
			log.Error(err, "failed to get node annotations")
			return ctrl.Result{}, err
		}

		ke, err := registry.NewKubernetesEndpoint(rc.clusterName, pod.Annotations, pod.Labels, pod.Status.PodIP, nodeLabels)
		if err != nil {
			log.Error(err, "failed to create Kubernetes endpoint")
			return ctrl.Result{}, err
		}

		if err = rc.reg.RegisterEndpoint(ctx, ke); err != nil {
			log.Error(err, "failed to register pod")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (rc *RegistrarController) getNodeLabels(ctx context.Context, pod *corev1.Pod) (map[string]string, error) {
	node, err := rc.getNode(ctx, pod)
	if err != nil {
		return nil, err
	}

	if node == nil {
		return make(map[string]string), nil
	}

	return node.Labels, nil
}

func (rc *RegistrarController) getNode(ctx context.Context, pod *corev1.Pod) (*corev1.Node, error) {
	if pod.Spec.NodeName == "" {
		return nil, nil
	}

	var node corev1.Node
	if err := rc.client.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", pod.Spec.NodeName, err)
	}

	return &node, nil
}

func getPodIPs(pod corev1.Pod) []string {
	var ips []string
	if pod.Status.PodIP != "" {
		ips = append(ips, pod.Status.PodIP)
	}
	for _, podIP := range pod.Status.PodIPs {
		if podIP.IP != "" && podIP.IP != pod.Status.PodIP {
			ips = append(ips, podIP.IP)
		}
	}
	return ips
}
