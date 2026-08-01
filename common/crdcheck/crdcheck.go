// Package crdcheck answers "is this CRD installed?" for controllers that watch
// OPTIONAL types. controller-runtime hard-crashes the manager when a watch's
// cache cannot sync because the CRD is absent (the install-ordering crashloop),
// so reconcilers gate each optional watch — and the corresponding List in
// Reconcile — on the type actually being served, degrading with a warning
// instead of wedging. Detection is at setup time: installing the CRD later
// requires an agent restart to pick it up.
package crdcheck

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Present reports whether gvk is served by the API server per the mapper.
// A NoMatch is the graceful "CRD not installed" answer; any other error is a
// real lookup failure and is returned.
func Present(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (bool, error) {
	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking %s availability: %w", gvk.Kind, err)
	}
	return true, nil
}
