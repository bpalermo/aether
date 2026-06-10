package server

import (
	"context"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRunResubscribeStoredPods(t *testing.T) {
	t.Run("nil bridge returns immediately", func(t *testing.T) {
		srv := newTestCNIServer(fake.NewClientBuilder().Build(), storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{}, cache.NewSnapshotCache("test-node", logr.Discard()), "")
		srv.spireBridge = nil

		done := make(chan struct{})
		go func() {
			srv.runResubscribeStoredPods(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runResubscribeStoredPods did not return with a nil bridge")
		}
	})

	t.Run("returns when context is canceled before the bridge starts", func(t *testing.T) {
		srv := newTestCNIServer(fake.NewClientBuilder().Build(), storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{}, cache.NewSnapshotCache("test-node", logr.Discard()), "")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		go func() {
			srv.runResubscribeStoredPods(ctx)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runResubscribeStoredPods did not return on canceled context while waiting for the bridge")
		}
	})
}
