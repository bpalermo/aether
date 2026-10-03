package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/registry/backend"
)

// endpointOf returns the endpoint snap lists for ip under service, or nil.
func endpointOf(snap *Snapshot, service, ip string) *registryv1.ServiceEndpoint {
	eps, _ := snap.GetAllWithVersion(registryv1.Service_PROTOCOL_HTTP)
	for _, ep := range eps[service] {
		if ep.GetIp() == ip {
			return ep
		}
	}
	return nil
}

// TestReplicasAgreeOnAnEndpointAnAgentRegisteredOnOne is aether#1145 on the
// kubernetes registry backend, two registrar replicas.
//
// The backend ignores writes, so the agent's register op "flushes" as a no-op,
// and the write-behind overlay used to release it only once the pod-derived
// listing came back proto.Equal to the agent's endpoint. The agent's view and
// the pod-derived view are built by different code (the CNI registration path
// vs podToEndpoint) and differ in fields the pod view does not reproduce --
// here the health-check mode, which the CNI path defaults to EDS and this
// backend to UNSPECIFIED. The op was therefore never released: the receiving
// replica served the agent's version of the endpoint for as long as the pod
// lived, every other replica the pod's.
//
// Red on main: replica A keeps the agent's endpoint through every sync.
func TestReplicasAgreeOnAnEndpointAnAgentRegisteredOnOne(t *testing.T) {
	const (
		service = "ns/svc"
		ip      = "10.0.0.1"
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.DiscardHandler)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "ns",
			Labels: map[string]string{aetherlabels.LabelAetherManaged: "true"},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "svc", NodeName: "node"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	c := fake.NewClientBuilder().WithObjects(pod, node).Build()
	informer := controllertest.NewFakeInformer()

	type replica struct {
		snap *Snapshot
		srv  *RegistrarServer
	}
	newReplica := func() replica {
		reg, err := backend.New(ctx, log, "kubernetes", backend.Config{
			ClusterName: "c", Reader: c, Informers: podInformerSource{informer},
		})
		require.NoError(t, err)
		require.NoError(t, reg.Initialize(ctx))
		snap := NewSnapshot()
		bc := NewBroadcaster(log, nil)
		// A short poll stands in for the 5 s default; the pod does not change,
		// so the informer never fires and only polls can release the op.
		syncer := NewSyncer(reg, snap, bc, 50*time.Millisecond, log, nil)
		srv := NewRegistrarServer(reg, snap, bc, "127.0.0.1:0", log, nil)
		wb := NewWriteBehindQueue(reg, log, nil)
		srv.UseWriteBehind(wb)
		syncer.UseWriteBehind(wb)
		go func() { _ = syncer.Start(ctx) }()
		go func() { _ = wb.Start(ctx) }()
		requireSynced(t, syncer)
		return replica{snap: snap, srv: srv}
	}
	a, b := newReplica(), newReplica()
	podView := endpointOf(b.snap, service, ip)
	require.NotNil(t, podView, "replica B never listed the pod")

	// The agent's registration, as the CNI path builds it: same pod, same
	// health, but a field the pod-derived view does not reproduce.
	agentView := proto.Clone(podView).(*registryv1.ServiceEndpoint)
	agentView.HealthCheckMode = registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
	require.False(t, proto.Equal(agentView, podView), "the test needs the two views to differ")
	_, err := a.srv.RegisterEndpoint(ctx, &registrarv1.RegisterEndpointRequest{
		ServiceName: service, Protocol: registryv1.Service_PROTOCOL_HTTP, Endpoint: agentView,
	})
	require.NoError(t, err)

	// Many polls later, both replicas must serve the same endpoint.
	require.Eventually(t, func() bool {
		return proto.Equal(endpointOf(a.snap, service, ip), endpointOf(b.snap, service, ip))
	}, 2*time.Second, 10*time.Millisecond,
		"replica A still serves the agent's version of the endpoint, replica B the pod's")
	require.True(t, proto.Equal(podView, endpointOf(a.snap, service, ip)),
		"on the kubernetes backend the pod-derived view is the source of truth")
}
