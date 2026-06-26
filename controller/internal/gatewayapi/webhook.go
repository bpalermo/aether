// Package gatewayapi provides the aether-controller's Gateway API HTTPRoute
// validating admission webhook. Gateway API allows multiple HTTPRoutes to share a
// hostname on one Gateway — the data plane merges their routes in precedence order
// (creationTimestamp/namespace/name tie-break, then path specificity). The webhook
// therefore admits same-hostname routes freely. It retains only the structural
// checks required for correctness: routes with no parentRefs (unreachable) and
// invalid JSON are the current no-ops / hard-errors. See
// docs/proposals/018_gateway-api-gamma.md and proposal 021 for the hostname-merge
// design.
package gatewayapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteKind is the Gateway API Kind the controller's shared /validate
// dispatcher routes to this validator.
const HTTPRouteKind = "HTTPRoute"

// Validator is an admission webhook for HTTPRoutes. It admits all structurally
// valid HTTPRoutes. Duplicate-hostname rejection (previously here) was dropped
// when the data plane gained same-hostname route merge: multiple HTTPRoutes
// sharing a hostname on one Gateway now merge in path-specificity / creation-
// timestamp order rather than conflicting.
//
// The Reader field is kept for forward-compatibility (future validations that
// need to list existing routes), but is unused in the current implementation.
type Validator struct {
	// Reader lists existing HTTPRoutes cluster-wide. Retained for
	// forward-compatibility; not used in the current (admit-all) implementation.
	Reader client.Reader
	Log    *slog.Logger
}

// Handle admits all structurally-valid HTTPRoutes. The webhook is kept in the
// admission chain so the controller's /validate dispatcher still routes
// HTTPRoute requests here; extending validation in the future does not require
// registering a new webhook path.
func (v *Validator) Handle(_ context.Context, req admission.Request) admission.Response {
	hr := &gatewayv1.HTTPRoute{}
	if err := json.Unmarshal(req.Object.Raw, hr); err != nil {
		return admission.Denied(fmt.Sprintf("HTTPRoute is invalid: %v", err))
	}
	return admission.Allowed("")
}
