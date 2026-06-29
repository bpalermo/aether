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

	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/extensionfilter"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator is served by the controller's shared /validate dispatcher, keyed by the
// HTTPFilter Kind.
type Validator struct {
	Log *slog.Logger
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
	return admission.Allowed("")
}
