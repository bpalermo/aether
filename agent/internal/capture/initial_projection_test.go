package capture

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestInitialProjection_OneEventThatProjectsAnEmptyMesh is #1094's guarantee
// that the xDS server's first-serve hold on the projection cannot cost a node
// with no mesh Services the whole bound: the controller gets exactly one
// synthetic event, and reconciling it projects (an empty set is a projection).
func TestInitialProjection_OneEventThatProjectsAnEmptyMesh(t *testing.T) {
	ch := initialProjection()
	require.Len(t, ch, 1, "exactly one synthetic event")
	ev := <-ch
	require.NotNil(t, ev.Object)
	require.NotEmpty(t, ev.Object.GetName(), "EnqueueRequestForObject needs a valid key")

	sink := &fakeSink{}
	r := &Reconciler{Client: fake.NewClientBuilder().Build(), Sink: sink, Log: slog.New(slog.DiscardHandler)}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	require.NotNil(t, sink.got, "the reconcile must reach the sink even with no mesh Services")
}
