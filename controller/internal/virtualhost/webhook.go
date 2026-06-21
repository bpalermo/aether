// Package virtualhost provides the aether-controller's VirtualHost validating
// admission webhook: it runs protovalidate on the spec and rejects a CREATE/
// UPDATE whose hostnames collide with another VirtualHost (duplicate FQDN), so a
// collision never reaches the edge's data plane. See
// docs/proposals/017_virtual-host-l7-routing.md.
package virtualhost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"buf.build/go/protovalidate"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator is an admission webhook that rejects a VirtualHost whose spec fails
// protovalidate, or any of whose hostnames is already claimed by another
// VirtualHost cluster-wide (each external FQDN may be served by exactly one). It
// is served by the controller's shared /validate dispatcher (see
// controller/internal/webhook), keyed by the VirtualHost Kind.
type Validator struct {
	// Reader lists existing VirtualHosts cluster-wide for the duplicate-FQDN
	// check. The API reader (uncached) is used so the check is correct regardless
	// of the manager cache's scope, at the cost of one List per admission — fine,
	// VirtualHost writes are infrequent.
	Reader client.Reader
	Log    *slog.Logger
}

// Handle validates the incoming VirtualHost's spec and host uniqueness.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	vh := &crdv1.VirtualHost{}
	// Decode via the typed object's jsonshim (protojson on .spec), so an unknown
	// or malformed spec field is rejected here rather than reaching the edge.
	if err := json.Unmarshal(req.Object.Raw, vh); err != nil {
		return admission.Denied(fmt.Sprintf("VirtualHost spec is invalid: %v", err))
	}

	// Structural/semantic validation: hosts pattern + uniqueness within the
	// manifest, >=1 route, the path oneof, backend service, ... (CEL constraints
	// that the permissive CRD openAPI can't express).
	if vh.Spec != nil {
		if err := protovalidate.Validate(vh.Spec); err != nil {
			v.Log.InfoContext(ctx, "rejected invalid VirtualHost", "name", vh.GetName(), "error", err)
			return admission.Denied(err.Error())
		}
	}

	incoming := vh.Spec.GetHosts()
	if len(incoming) == 0 {
		return admission.Allowed("")
	}

	// Duplicate-FQDN across manifests: no host may be claimed by ANOTHER
	// VirtualHost cluster-wide (exact-string match; *.suffix vs a specific host is
	// not a conflict — Envoy resolves most-specific-first).
	list := &crdv1.VirtualHostList{}
	if err := v.Reader.List(ctx, list); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("list VirtualHosts: %w", err))
	}
	claimed := make(map[string]string, len(list.Items)) // host -> "namespace/name"
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == vh.Namespace && other.Name == vh.Name {
			continue // self (the UPDATE case)
		}
		for _, h := range other.Spec.GetHosts() {
			claimed[h] = other.Namespace + "/" + other.Name
		}
	}
	for _, h := range incoming {
		if owner, ok := claimed[h]; ok {
			v.Log.InfoContext(ctx, "rejected duplicate VirtualHost host", "name", vh.GetName(), "host", h, "owner", owner)
			return admission.Denied(fmt.Sprintf(
				"host %q is already claimed by VirtualHost %s; each external FQDN may be served by only one VirtualHost", h, owner))
		}
	}
	return admission.Allowed("")
}
