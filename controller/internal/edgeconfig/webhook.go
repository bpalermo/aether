// Package edgeconfig provides the admission validator for EdgeConfig resources
// (proposal 029): it proto-validates the spec so a CR can never describe a config
// the edge would refuse. Served by the controller's shared /validate dispatcher,
// keyed by the EdgeConfig Kind.
package edgeconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Validator rejects EdgeConfig resources whose spec fails protovalidate.
type Validator struct {
	Log *slog.Logger
}

// Handle validates the incoming EdgeConfig's spec.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	ec := &crdv1.EdgeConfig{}
	if err := json.Unmarshal(req.Object.Raw, ec); err != nil {
		return admission.Denied(fmt.Sprintf("EdgeConfig spec is invalid: %v", err))
	}
	if err := Validate(ec.Spec); err != nil {
		v.Log.InfoContext(ctx, "rejected invalid EdgeConfig", "name", ec.GetName(), "error", err)
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// Validate proto-validates an EdgeConfig spec. A nil spec is valid (pure defaults).
func Validate(spec *configv1.EdgeConfigSpec) error {
	if spec == nil {
		return nil
	}
	if err := protovalidate.Validate(spec); err != nil {
		return fmt.Errorf("EdgeConfig spec failed validation: %w", err)
	}
	return nil
}
