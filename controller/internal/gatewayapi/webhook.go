// Package gatewayapi provides the aether-controller's Gateway API HTTPRoute
// validating admission webhook: it rejects a CREATE/UPDATE whose hostnames
// collide with another HTTPRoute attached to the SAME Gateway (a duplicate FQDN
// on a shared edge listener), so the collision never reaches the edge's data
// plane. It supersedes the VirtualHost duplicate-FQDN webhook. See
// docs/proposals/018_gateway-api-gamma.md.
package gatewayapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteKind is the Gateway API Kind the controller's shared /validate
// dispatcher routes to this validator.
const HTTPRouteKind = "HTTPRoute"

// Validator is an admission webhook that rejects an HTTPRoute any of whose
// hostnames is already claimed by ANOTHER HTTPRoute attached to the same Gateway
// (each external FQDN may be served by exactly one route on a given listener). It
// is served by the controller's shared /validate dispatcher (see
// controller/internal/webhook), keyed by the HTTPRoute Kind.
//
// Two HTTPRoutes may share a hostname only if they attach to DIFFERENT Gateways
// (distinct edge listeners); a wildcard (*.suffix) and a specific host are not a
// conflict — Envoy resolves most-specific-first, exactly as the VirtualHost
// webhook did.
type Validator struct {
	// Reader lists existing HTTPRoutes cluster-wide for the duplicate-FQDN
	// check. The API reader (uncached) is used so the check is correct regardless
	// of the manager cache's scope, at the cost of one List per admission — fine,
	// HTTPRoute writes are infrequent.
	Reader client.Reader
	Log    *slog.Logger
}

// Handle validates the incoming HTTPRoute's hostname uniqueness per Gateway.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	hr := &gatewayv1.HTTPRoute{}
	if err := json.Unmarshal(req.Object.Raw, hr); err != nil {
		return admission.Denied(fmt.Sprintf("HTTPRoute is invalid: %v", err))
	}

	incoming := hr.Spec.Hostnames
	if len(incoming) == 0 {
		// A hostname-less HTTPRoute matches by path on whatever its Gateway
		// listener allows; there is no FQDN to collide.
		return admission.Allowed("")
	}

	// The set of Gateways this route attaches to (namespace/name). A collision is
	// only a conflict when both routes share at least one Gateway.
	incomingGateways := gatewaySet(hr)
	if len(incomingGateways) == 0 {
		return admission.Allowed("")
	}

	list := &gatewayv1.HTTPRouteList{}
	if err := v.Reader.List(ctx, list); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("list HTTPRoutes: %w", err))
	}

	// host -> "namespace/name" of the route already claiming it on a SHARED Gateway.
	claimed := make(map[string]string)
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == hr.Namespace && other.Name == hr.Name {
			continue // self (the UPDATE case)
		}
		if !sharesGateway(incomingGateways, gatewaySet(other)) {
			continue // different Gateway/listener: not a conflict
		}
		for _, h := range other.Spec.Hostnames {
			claimed[string(h)] = other.Namespace + "/" + other.Name
		}
	}
	for _, h := range incoming {
		if owner, ok := claimed[string(h)]; ok {
			v.Log.InfoContext(ctx, "rejected duplicate HTTPRoute host", "name", hr.GetName(), "host", string(h), "owner", owner)
			return admission.Denied(fmt.Sprintf(
				"host %q is already claimed by HTTPRoute %s on the same Gateway; each external FQDN may be served by only one HTTPRoute per Gateway", string(h), owner))
		}
	}
	return admission.Allowed("")
}

// gatewaySet returns the set of Gateways ("namespace/name") an HTTPRoute attaches
// to via its parentRefs. A parentRef with no namespace defaults to the route's
// own namespace. Non-Gateway parentRefs are ignored.
func gatewaySet(hr *gatewayv1.HTTPRoute) map[string]struct{} {
	set := map[string]struct{}{}
	for _, p := range hr.Spec.ParentRefs {
		if p.Group != nil && string(*p.Group) != gatewayv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		ns := hr.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		set[ns+"/"+string(p.Name)] = struct{}{}
	}
	return set
}

// sharesGateway reports whether two Gateway sets intersect.
func sharesGateway(a, b map[string]struct{}) bool {
	for g := range a {
		if _, ok := b[g]; ok {
			return true
		}
	}
	return false
}
