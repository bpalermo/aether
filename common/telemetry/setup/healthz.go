package setup

import (
	"context"
	"errors"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// cacheSyncCheckTimeout bounds the readiness check's wait for the informer
// cache. WaitForCacheSync blocks until the cache syncs or the request context
// ends, so an unsynced cache used to hang the probe until the kubelet's own
// 1s timeout expired — reported as "Readiness probe failed: context deadline
// exceeded", which is indistinguishable from a wedged process (issue #662).
// Answering below the probe timeout turns that into an honest "not synced yet".
const cacheSyncCheckTimeout = 500 * time.Millisecond

// CacheSyncChecker returns a healthz.Checker that reports healthy once the
// controller-runtime informer cache has synced, and reports unready promptly
// (rather than hanging the probe) while it has not.
func CacheSyncChecker(mgr ctrl.Manager) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), cacheSyncCheckTimeout)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("informer cache not synced")
		}
		return nil
	}
}
