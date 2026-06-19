package meshconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// WebhookPath is the URL path the ValidatingWebhookConfiguration points at.
// Unversioned: the proto spec's field-number evolution guarantees compatibility,
// so the endpoint never needs a version suffix.
const WebhookPath = "/validate"

// Validator is an admission webhook that rejects MeshConfig resources whose spec
// fails protovalidate — the same check the agent's file loader and the reconciler
// run, so a CR can never describe a config the binaries would refuse to load.
type Validator struct {
	Log *slog.Logger
}

// SetupWithManager registers the validating webhook on the manager's webhook
// server.
func (v *Validator) SetupWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(WebhookPath, &admission.Webhook{Handler: v})
}

// Handle validates the incoming MeshConfig's spec.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	mc := &configv1.MeshConfig{}
	// Decode via the typed object's jsonshim (protojson on .spec), so an unknown
	// or malformed spec field is rejected here rather than reaching the binaries.
	if err := json.Unmarshal(req.Object.Raw, mc); err != nil {
		return admission.Denied(fmt.Sprintf("MeshConfig spec is invalid: %v", err))
	}
	if err := Validate(mc.Spec); err != nil {
		v.Log.InfoContext(ctx, "rejected invalid MeshConfig", "name", mc.GetName(), "error", err)
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}
