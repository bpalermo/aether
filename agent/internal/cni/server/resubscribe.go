package server

import (
	"context"

	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runResubscribeStoredPods restores SVID subscriptions for pods already present
// in local storage when the agent (re)starts. Subscriptions are normally created
// only on CNI ADD, so without this an agent restart rebuilds listeners from
// storage but leaves existing pods' workload SVIDs unsubscribed — their mTLS
// breaks ("Secret is not supplied by SDS") until the pods are recreated.
//
// It waits for the SPIRE bridge to connect (SubscribePod no-ops before then),
// then re-subscribes every managed stored pod, re-fetching the pod UID from the
// API server (the UID is needed for the SPIRE k8s:pod-uid selector and is not
// persisted in storage). Idempotent: SubscribePod no-ops for an
// already-subscribed network namespace, so racing a concurrent CNI ADD is safe.
// Pods deleted while the agent was down fail the UID lookup and are skipped;
// their CNI DEL cleans them up.
func (s *CNIServer) runResubscribeStoredPods(ctx context.Context) {
	if s.spireBridge == nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-s.spireBridge.Started():
	}

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.Error(err, "resubscribe: failed to list stored pods")
		return
	}

	resubscribed := 0
	for _, pod := range pods {
		if isIgnorablePod(pod) {
			continue
		}
		log := s.log.WithValues("pod", pod.GetName(), "namespace", pod.GetNamespace())

		var k8sPod corev1.Pod
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: pod.GetNamespace(), Name: pod.GetName()}, &k8sPod); err != nil {
			log.V(1).Info("resubscribe: pod not found in API server; skipping", "error", err)
			continue
		}

		spiffeID := proxy.SpiffeIDFromPod(pod, s.trustDomain)
		selectors := spire.PodSelectors(pod.GetNamespace(), pod.GetServiceAccount(), pod.GetName(), string(k8sPod.UID))
		if err := s.spireBridge.SubscribePod(pod.GetNetworkNamespace(), spiffeID, selectors); err != nil {
			log.Error(err, "resubscribe: failed to subscribe SVID", "spiffeID", spiffeID)
			continue
		}
		resubscribed++
		log.V(1).Info("resubscribed stored pod SVID", "spiffeID", spiffeID)
	}

	s.log.Info("resubscribed stored pod SVIDs", "count", resubscribed, "stored", len(pods))
}
