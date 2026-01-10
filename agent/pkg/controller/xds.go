package controller

import (
	"context"
	"sync"
	"time"

	"github.com/bpalermo/aether/agent/pkg/constants"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type Op uint32

const (
	AddPod Op = 1 << iota
	DeletePod
)

type Event struct {
	Op  Op
	Pod *corev1.Pod
}

type XdsController struct {
	mgr ctrl.Manager

	logger logr.Logger

	client      client.Client
	podInformer cache.Informer

	initWg    *sync.WaitGroup
	eventChan chan<- *registryv1.Event

	ready bool
}

func NewXdsController(ctx context.Context, mgr ctrl.Manager, initWg *sync.WaitGroup, eventChan chan<- *registryv1.Event, logger logr.Logger) (*XdsController, error) {
	podInformer, err := mgr.GetCache().GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		return nil, err
	}

	return &XdsController{
		mgr,
		logger.WithName("xds-controller"),
		mgr.GetClient(),
		podInformer,
		initWg,
		eventChan,
		false,
	}, nil
}

func (c *XdsController) SetupWithManager(mgr ctrl.Manager) error {
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
		Complete(c)
}

func (c *XdsController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Check sync status first before fetching the pod
	if !c.ready && c.podInformer.HasSynced() {
		c.ready = true
		c.initWg.Done()
		c.logger.Info("pod informer has synced, ready to process")
	}

	resourceEvent := &registryv1.Event_K8SPod{
		K8SPod: &registryv1.KubernetesPod{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	// Fetch pod after sync check
	pod := &corev1.Pod{}
	if err := c.client.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			// Pod was deleted - send delete event
			c.logger.Info("pod not found, sending delete event", "pod", req.Name, "namespace", req.Namespace)
			// write DELETE operation to the event channel
			event := &registryv1.Event{
				Operation: registryv1.Event_DELETED,
				Resource:  resourceEvent,
			}

			select {
			case c.eventChan <- event:
				c.logger.Info("sent delete event", "pod", req.Name, "namespace", req.Namespace)
			case <-ctx.Done():
				return ctrl.Result{}, ctx.Err()
			default:
				c.logger.Info("event channel full, re-queuing delete", "pod", req.Name, "namespace", req.Namespace)
				return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
			}

			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Determine the operation type
	op := registryv1.Event_CREATED
	if !pod.DeletionTimestamp.IsZero() {
		op = registryv1.Event_DELETED
		c.logger.Info("pod is marked for deletion", "pod", req.Name, "namespace", req.Namespace)
	}

	if pod.Status.PodIP == "" {
		c.logger.Info("pod IP not yet available", "pod", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	// At this point we have a pod
	resourceEvent.K8SPod.Ip = pod.Status.PodIP
	resourceEvent.K8SPod.ServiceName = pod.Labels[constants.AetherServiceLabel]

	event := &registryv1.Event{
		Operation: op,
		Resource:  resourceEvent,
	}

	// Send event with context awareness
	select {
	case c.eventChan <- event:
		c.logger.Info("sent event", "pod", req.Name, "namespace", req.Namespace, "operation", op.String())
	case <-ctx.Done():
		return ctrl.Result{}, ctx.Err()
	default:
		// Channel is full, requeue
		c.logger.Info("event channel full, re-queuing", "pod", req.Name, "namespace", req.Namespace, "op", op.String())
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	return ctrl.Result{}, nil
}
