// Package endpointpolicy holds the controller's admission webhook for the
// EndpointPolicy CRD (proposal 034 Phase 1b, service-scoped UDS delivery). It runs
// the agent's own resolver over the declared socket, so a value the data plane
// would refuse — a bad path segment, or one that overflows the AF_UNIX sun_path
// budget — is rejected at apply time instead of degrading one service to TCP with
// an error log on every node that hosts it.
package endpointpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"buf.build/go/protovalidate"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/udspath"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// uidPlaceholder stands in for the pod UID the agent substitutes at resolution
// time. Kubernetes UIDs are RFC 4122 strings, so 36 bytes is both the worst case
// and the only case; using it here makes the webhook's budget check exact.
const uidPlaceholder = 36

// Validator is served by the controller's shared /validate dispatcher, keyed by
// the EndpointPolicy Kind.
type Validator struct {
	Log *slog.Logger
}

// Handle validates the incoming EndpointPolicy's spec.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	ep := &crdv1.EndpointPolicy{}
	// Decode via the typed object's jsonshim (protojson on .spec) so a malformed
	// spec is rejected here rather than silently dropped.
	if err := json.Unmarshal(req.Object.Raw, ep); err != nil {
		return admission.Denied(fmt.Sprintf("EndpointPolicy spec is invalid: %v", err))
	}
	if err := Validate(ep.Spec); err != nil {
		v.Log.InfoContext(ctx, "rejected invalid EndpointPolicy", "name", ep.GetName(), "namespace", ep.GetNamespace(), "error", err)
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// Validate checks an EndpointPolicy spec: proto rules (targetRef shape, socket
// shape) plus the socket's resolvability. A nil spec is rejected — an
// EndpointPolicy that declares nothing is always an authoring mistake.
func Validate(spec *configv1.EndpointPolicySpec) error {
	if spec == nil {
		return fmt.Errorf("EndpointPolicy spec is required")
	}
	if err := protovalidate.Validate(spec); err != nil {
		return fmt.Errorf("EndpointPolicy spec failed validation: %w", err)
	}
	return validateSocket(spec.GetUdsSocket())
}

// validateSocket runs the agent's resolver over the declared socket with a
// worst-case pod UID, so admission enforces exactly the segment rules and the
// 107-byte sun_path budget the data plane enforces.
//
// The budget is computed against the DEFAULT kubelet pods directory. A cluster
// running the agent with a nonstandard --kubelet-pods-dir shifts it either way,
// which is why the agent still resolves fail-closed at listener-generation time
// (falling back to TCP delivery) instead of trusting this check.
func validateSocket(socket string) error {
	if _, err := udspath.Resolve(udspath.DefaultKubeletPodsDir, strings.Repeat("0", uidPlaceholder), socket); err != nil {
		return fmt.Errorf("spec.udsSocket %q is not usable: %w", socket, err)
	}
	return nil
}
