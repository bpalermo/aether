package telemetry

import (
	"errors"
	"net/http"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// CacheSyncChecker returns a healthz.Checker that reports healthy once the
// controller-runtime informer cache has synced.
func CacheSyncChecker(mgr ctrl.Manager) healthz.Checker {
	return func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return errors.New("informer cache not synced")
		}
		return nil
	}
}
