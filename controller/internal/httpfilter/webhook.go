// Package httpfilter holds the controller's admission webhook for the HTTPFilter CRD
// (proposal 025, the proxy-extension escape hatch). It validates the opaque
// spec.typedConfig fail-closed, IN-PROCESS — no Envoy binary — by resolving the
// `@type` against the linked Envoy protos and running protoc-gen-validate, plus the
// filter allow-list. A bad escape-hatch payload is rejected at CRD apply instead of
// NACKing the proxy fleet (the Istio EnvoyFilter footgun, made fail-closed).
package httpfilter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/extensionfilter"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator is served by the controller's shared /validate dispatcher, keyed by the
// HTTPFilter Kind.
type Validator struct {
	// Reader lists existing HTTPFilters for the one-CHAIN-filter-per-service check
	// (025 M4). Use the manager's APIReader (uncached — correct regardless of cache
	// scope). Optional: when nil the dup check is skipped (the projector still
	// tie-breaks deterministically).
	Reader client.Reader
	Log    *slog.Logger
}

// Handle validates the incoming HTTPFilter's spec.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	hf := &crdv1.HTTPFilter{}
	// Decode via the typed object's jsonshim (protojson on .spec, opaque-Any @type),
	// so an unknown/malformed spec or typedConfig is rejected here.
	if err := json.Unmarshal(req.Object.Raw, hf); err != nil {
		return admission.Denied(fmt.Sprintf("HTTPFilter spec is invalid: %v", err))
	}
	if err := extensionfilter.ValidateSpec(hf.Spec); err != nil {
		v.Log.InfoContext(ctx, "rejected invalid HTTPFilter", "name", hf.GetName(), "error", err)
		return admission.Denied(err.Error())
	}
	// One CHAIN-scope filter per service (025 M4): the service-wide always-on slot is
	// singular by design — no merge/ordering semantics across filters. Reject a second
	// CHAIN filter targeting a service another HTTPFilter (different name) already
	// claims. The shared projector's deterministic tie-break is belt-and-braces for
	// races the webhook can't see.
	if hf.Spec.GetScope() == configprotov1.HTTPFilterSpec_SCOPE_CHAIN && v.Reader != nil {
		if resp := v.checkChainConflict(ctx, hf); resp != nil {
			return *resp
		}
	}
	return admission.Allowed("")
}

// checkChainConflict returns a denial when another CHAIN-scope HTTPFilter in the
// namespace targets one of hf's target services; nil when admissible.
func (v *Validator) checkChainConflict(ctx context.Context, hf *crdv1.HTTPFilter) *admission.Response {
	targets := map[string]struct{}{}
	for _, t := range hf.Spec.GetTargetRefs() {
		if (t.GetGroup() == "" || t.GetGroup() == "core") && t.GetKind() == "Service" {
			targets[t.GetName()] = struct{}{}
		}
	}
	list := &crdv1.HTTPFilterList{}
	if err := v.Reader.List(ctx, list, client.InNamespace(hf.GetNamespace())); err != nil {
		// Fail-closed would block all CHAIN applies on a transient list error; the
		// projector tie-break keeps the data plane deterministic, so warn + allow.
		v.Log.WarnContext(ctx, "chain-conflict check skipped: list failed", "error", err)
		return nil
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.GetName() == hf.GetName() || other.Spec == nil ||
			other.Spec.GetScope() != configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
			continue
		}
		for _, t := range other.Spec.GetTargetRefs() {
			if (t.GetGroup() != "" && t.GetGroup() != "core") || t.GetKind() != "Service" {
				continue
			}
			if _, clash := targets[t.GetName()]; clash {
				resp := admission.Denied(fmt.Sprintf(
					"service %q already has a CHAIN-scope HTTPFilter (%q); at most one service-wide filter per service",
					t.GetName(), other.GetName()))
				return &resp
			}
		}
	}
	return nil
}
