package meshconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator is an admission webhook that rejects MeshConfig resources whose spec
// fails protovalidate — the same check the agent's file loader and the reconciler
// run, so a CR can never describe a config the binaries would refuse to load. It
// is served by the controller's shared /validate dispatcher (see
// controller/internal/webhook), keyed by the MeshConfig Kind.
type Validator struct {
	Log *slog.Logger
}

// Handle validates the incoming MeshConfig's spec.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	mc := &crdv1.MeshConfig{}
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
