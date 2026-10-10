package server

import (
	"context"
	"errors"
	"fmt"

	"aethermesh.dev/agent/internal/spire"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReconcileStorage is the CNI half of a surge takeover (proposal 041): it
// applies what the previous owner of the node did to local storage while this
// agent was a standby, so the first snapshot the node's proxy gets from this
// agent matches the pods actually on the node.
//
// During the overlap the old agent served every CNI ADD and DEL: it wrote the
// pod records, (de)registered the endpoints and pushed the listeners to the
// proxy it was serving. This agent loaded storage at its start and has been
// building from that view. So, under lifecycleMu and before cni.sock is bound
// (the takeover runs before ownership is announced):
//
//   - a pod ADDed during the overlap gets its listeners and its SVID
//     subscription — the registry already has its endpoint;
//   - a pod DELed during the overlap loses its listeners and its subscription
//     — the registry already lost its endpoint, and a listener left behind
//     would point the proxy at a dead netns until the ghost sweep noticed;
//   - a pod record rewritten during the overlap (the old agent's termination
//     watch marking it Terminating) is rebuilt from the new record.
//
// It is a diff, not a reload: rebuilding every listener costs 0.25-0.98 s on
// the reference cluster, inside the window the proxy has no ADS stream (open question 1).
func (s *CNIServer) ReconcileStorage(ctx context.Context) error {
	reloader, ok := s.storage.(storage.Reloader[*cniv1.CNIPod])
	if !ok {
		return nil
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	delta, err := reloader.Reload(ctx)
	if err != nil {
		return fmt.Errorf("reloading local storage: %w", err)
	}
	if delta.Empty() {
		s.log.InfoContext(ctx, "takeover: local storage unchanged during the overlap")
		return nil
	}

	errs := s.takeOverRemoved(ctx, delta.Removed)
	errs = append(errs, s.takeOverPresent(ctx, delta.Updated, false)...)
	errs = append(errs, s.takeOverPresent(ctx, delta.Added, true)...)

	s.log.InfoContext(ctx, "takeover: applied the previous agent's CNI ADD/DEL from the overlap",
		"added", len(delta.Added), "updated", len(delta.Updated), "removed", len(delta.Removed))
	return errors.Join(errs...)
}

// takeOverRemoved tears down what this agent built for pods the previous
// agent DELed during the overlap: their listeners and SVID subscriptions.
func (s *CNIServer) takeOverRemoved(ctx context.Context, pods []*cniv1.CNIPod) []error {
	var errs []error
	for _, pod := range pods {
		if isIgnorablePod(pod) {
			continue
		}
		netns := pod.GetNetworkNamespace()
		if err := s.snapshotInstalled(ctx, podLog(s.log, pod), snapshotCallerTakeover, s.snapshotCache.RemovePod(ctx, netns)); err != nil {
			errs = append(errs, fmt.Errorf("removing listeners of %s/%s: %w", pod.GetNamespace(), pod.GetName(), err))
		}
		if s.spireBridge == nil {
			continue
		}
		if err := s.spireBridge.UnsubscribePod(ctx, netns); err != nil {
			errs = append(errs, fmt.Errorf("unsubscribing %s/%s: %w", pod.GetNamespace(), pod.GetName(), err))
		}
	}
	return errs
}

// takeOverPresent (re)builds the listeners of pods the previous agent ADDed
// (added) or rewrote during the overlap, subscribing the added ones' SVIDs.
func (s *CNIServer) takeOverPresent(ctx context.Context, pods []*cniv1.CNIPod, added bool) []error {
	var errs []error
	for _, pod := range pods {
		if isIgnorablePod(pod) {
			continue
		}
		if err := s.snapshotInstalled(ctx, podLog(s.log, pod), snapshotCallerTakeover, s.snapshotCache.AddPod(ctx, pod, s.trustDomain)); err != nil {
			errs = append(errs, fmt.Errorf("building listeners of %s/%s: %w", pod.GetNamespace(), pod.GetName(), err))
		}
		if added {
			s.subscribeTakenOverPod(ctx, pod)
		}
	}
	return errs
}

// subscribeTakenOverPod subscribes a pod the previous agent ADDed during the
// overlap to its SVID, as AddPod would have. The UID is persisted on the
// record (proposal 034); a record written before that falls back to the API.
func (s *CNIServer) subscribeTakenOverPod(ctx context.Context, pod *cniv1.CNIPod) {
	if s.spireBridge == nil {
		return
	}
	uid := pod.GetUid()
	if uid == "" && s.k8sClient != nil {
		var k8sPod corev1.Pod
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: pod.GetNamespace(), Name: pod.GetName()}, &k8sPod); err != nil {
			s.log.WarnContext(ctx, "takeover: pod ADDed during the overlap not found in the API server; not subscribing its SVID",
				"pod", pod.GetName(), "namespace", pod.GetNamespace(), "error", err)
			return
		}
		uid = string(k8sPod.UID)
	}
	spiffeID := proxy.SpiffeIDFromPod(pod, s.trustDomain)
	ref := spire.PodRef{Namespace: pod.GetNamespace(), Name: pod.GetName(), UID: uid}
	if err := s.spireBridge.SubscribePod(pod.GetNetworkNamespace(), spiffeID, ref); err != nil {
		s.log.ErrorContext(ctx, "takeover: failed to subscribe SVID", "pod", pod.GetName(), "namespace", pod.GetNamespace(), "error", err)
	}
}
