package endpointpolicy

import (
	"context"
	"log/slog"
	"testing"

	configv1 "aethermesh.dev/api/aether/config/v1"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// recordingSink captures the last projected map.
type recordingSink struct {
	policies map[string]string
	calls    int
}

func (s *recordingSink) SetUDSServicePolicies(policies map[string]string) {
	s.policies = policies
	s.calls++
}

func makePolicy(namespace, name, targetKind, targetGroup, targetName, socket string) configapisv1.EndpointPolicy {
	ep := configapisv1.EndpointPolicy{
		Spec: configv1.EndpointPolicySpec_builder{
			TargetRef: configv1.PolicyTargetRef_builder{Group: targetGroup, Kind: targetKind, Name: targetName}.Build(),
			UdsSocket: socket,
		}.Build(),
	}
	ep.Name = name
	ep.Namespace = namespace
	return ep
}

func testReconciler() *Reconciler {
	return &Reconciler{Log: slog.New(slog.DiscardHandler), enabled: true}
}

// TestProject verifies the "<ns>/<svc>" keying and that every unsupported
// attachment is skipped rather than projected under a wrong key.
func TestProject(t *testing.T) {
	r := testReconciler()
	items := []configapisv1.EndpointPolicy{
		makePolicy("team-a", "echo-uds", "Service", "", "echo", "s/app.sock"),
		makePolicy("team-b", "api-uds", "Service", "core", "api", "v/api.sock"),
		makePolicy("team-a", "wrong-kind", "Deployment", "apps", "worker", "s/app.sock"),
		makePolicy("team-a", "wrong-group", "Service", "apps", "other", "s/app.sock"),
		makePolicy("team-a", "no-socket", "Service", "", "quiet", ""),
		makePolicy("team-a", "no-target", "Service", "", "", "s/app.sock"),
	}
	// A policy with no spec at all must not panic or project.
	nilSpec := configapisv1.EndpointPolicy{}
	nilSpec.Name = "nil-spec"
	nilSpec.Namespace = "team-a"
	items = append(items, nilSpec)

	assert.Equal(t, map[string]string{
		"team-a/echo": "s/app.sock",
		"team-b/api":  "v/api.sock",
	}, r.project(context.Background(), items))
}

// TestProject_DuplicateTargetIsDeterministic pins the tie-break: two policies on
// one service resolve to the lexicographically smallest policy name, regardless of
// the order the API server lists them in.
func TestProject_DuplicateTargetIsDeterministic(t *testing.T) {
	r := testReconciler()
	first := makePolicy("team-a", "aaa-policy", "Service", "", "echo", "a/app.sock")
	second := makePolicy("team-a", "zzz-policy", "Service", "", "echo", "z/app.sock")

	want := map[string]string{"team-a/echo": "a/app.sock"}
	assert.Equal(t, want, r.project(context.Background(), []configapisv1.EndpointPolicy{first, second}))
	assert.Equal(t, want, r.project(context.Background(), []configapisv1.EndpointPolicy{second, first}),
		"the winner must not depend on list order")
}

// TestReconcile verifies the level-based projection: each reconcile re-lists and
// replaces the sink's whole map, so a deletion converges without delta tracking.
func TestReconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configapisv1.AddToScheme(scheme))

	policy := makePolicy("team-a", "echo-uds", "Service", "", "echo", "s/app.sock")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&policy).Build()

	sink := &recordingSink{}
	r := testReconciler()
	r.Client = c
	r.Sink = sink

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team-a/echo": "s/app.sock"}, sink.policies)

	require.NoError(t, c.Delete(context.Background(), &policy))
	_, err = r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Empty(t, sink.policies, "a removed policy leaves the service without one")
}

// TestReconcile_CRDAbsent verifies the reconciler is inert when the CRD is not
// served: no List (the nil Client would panic on one) and no sink write, so a
// cluster without the CRD never crashes the manager.
func TestReconcile_CRDAbsent(t *testing.T) {
	sink := &recordingSink{}
	r := &Reconciler{Log: slog.New(slog.DiscardHandler), Sink: sink}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Zero(t, sink.calls)
}
