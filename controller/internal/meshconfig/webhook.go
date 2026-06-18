package meshconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// WebhookPath is the URL path the ValidatingWebhookConfiguration points at.
const WebhookPath = "/validate-config-aether-io-v1-meshconfig"

// Validator is an admission webhook that rejects MeshConfig resources whose
// spec fails protovalidate — the same check the file loader and the registrar's
// self-config run, so a CR can never describe a config the binaries would
// refuse to load.
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
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, u); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode MeshConfig: %w", err))
	}

	spec, _, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("read MeshConfig spec: %w", err))
	}
	if _, err := ProtoFromSpec(spec); err != nil {
		v.Log.InfoContext(ctx, "rejected invalid MeshConfig", "name", u.GetName(), "error", err)
		return admission.Denied(err.Error())
	}

	return admission.Allowed("")
}
