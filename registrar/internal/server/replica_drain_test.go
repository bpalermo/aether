package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/registry/backend"
)

type podInformerSource struct{ informer *controllertest.FakeInformer }

func (p podInformerSource) GetInformer(context.Context, client.Object, ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	return p.informer, nil
}

// healthOf returns the health snap lists for ip under service, or false.
func healthOf(snap *Snapshot, service, ip string) (registryv1.ServiceEndpoint_Health, bool) {
	eps, _ := snap.GetAllWithVersion(registryv1.Service_PROTOCOL_HTTP)
	for _, ep := range eps[service] {
		if ep.GetIp() == ip {
			return ep.GetHealth(), true
		}
	}
	return registryv1.ServiceEndpoint_HEALTH_UNSPECIFIED, false
}

// TestPeerReplicaLearnsADrainWithoutWaitingForThePoll is aether#1124 on the
// kubernetes registry backend (the chart default), two registrar replicas.
//
// The destination agent writes its DRAINING mark to replica A, the one it is
// connected to. This backend ignores writes, so the mark never reaches the
// shared store and replica B, whose watchers include the sources, cannot hear
// it from A. B has to learn the drain from the Pod (its deletionTimestamp, the
// event the agent itself reacted to), and at watch speed: the poll interval
// here is an hour, so only the pod informer can deliver it inside the bound.
func TestPeerReplicaLearnsADrainWithoutWaitingForThePoll(t *testing.T) {
	const (
		service = "ns/svc"
		ip      = "10.0.0.1"
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.DiscardHandler)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "victim", Namespace: "ns",
			Labels: map[string]string{aetherlabels.LabelAetherManaged: "true"},
			// Lets the fake client keep the pod after Delete with a
			// deletionTimestamp, as the API server does through its grace period.
			Finalizers: []string{"test.aether.io/hold"},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "svc", NodeName: "node"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	c := fake.NewClientBuilder().WithObjects(pod, node).WithStatusSubresource(&corev1.Pod{}).Build()
	informer := controllertest.NewFakeInformer()

	type replica struct {
		snap   *Snapshot
		syncer *Syncer
		srv    *RegistrarServer
	}
	newReplica := func() replica {
		// Each replica is its own process with its own backend instance and
		// its own handler on (its own copy of) the pod informer.
		reg, err := backend.New(ctx, log, "kubernetes", backend.Config{
			ClusterName: "c", Reader: c, Informers: podInformerSource{informer},
		})
		require.NoError(t, err)
		require.NoError(t, reg.Initialize(ctx))
		snap := NewSnapshot()
		bc := NewBroadcaster(log, nil)
		syncer := NewSyncer(reg, snap, bc, time.Hour, log, nil)
		srv := NewRegistrarServer(reg, snap, bc, "127.0.0.1:0", log, nil)
		wb := NewWriteBehindQueue(reg, log, nil)
		srv.UseWriteBehind(wb)
		syncer.UseWriteBehind(wb)
		go func() { _ = syncer.Start(ctx) }()
		go func() { _ = wb.Start(ctx) }()
		requireSynced(t, syncer)
		return replica{snap: snap, syncer: syncer, srv: srv}
	}
	a, b := newReplica(), newReplica()
	for name, r := range map[string]replica{"A": a, "B": b} {
		h, ok := healthOf(r.snap, service, ip)
		require.True(t, ok, "replica %s never listed the pod", name)
		require.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, h, "replica %s", name)
	}

	// Deletion requested: the API server stamps the deletionTimestamp and the
	// informers deliver the update; the destination agent, reacting to the same
	// event, sends its DRAINING mark to replica A.
	before := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), before))
	require.NoError(t, c.Delete(ctx, pod))
	after := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), after))
	require.NotNil(t, after.DeletionTimestamp)
	marked := time.Now()
	informer.Update(before, after)
	_, err := a.srv.RegisterEndpoint(ctx, &registrarv1.RegisterEndpointRequest{
		ServiceName: service, Protocol: registryv1.Service_PROTOCOL_HTTP,
		Endpoint: &registryv1.ServiceEndpoint{Ip: ip, Health: registryv1.ServiceEndpoint_HEALTH_DRAINING},
	})
	require.NoError(t, err)

	h, _ := healthOf(a.snap, service, ip)
	require.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, h, "the receiving replica applies the mark on receipt")

	// Well inside one second, against an hour-long poll: only a push gets here.
	require.Eventually(t, func() bool {
		h, _ := healthOf(b.snap, service, ip)
		return h == registryv1.ServiceEndpoint_HEALTH_DRAINING
	}, time.Second, 10*time.Millisecond,
		"the peer replica never listed the terminating pod DRAINING; its watchers keep selecting it until its next poll (or, on a pod still Ready through its preStop, until the pod is gone)")
	t.Logf("peer replica DRAINING %v after the deletion request", time.Since(marked).Round(time.Millisecond))
}
