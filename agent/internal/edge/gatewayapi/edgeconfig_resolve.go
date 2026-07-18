package gatewayapi

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// edgeConfigGroup/Kind identify an EdgeConfig referent in a parametersRef.
const (
	edgeConfigGroup = "config.aether.io"
	edgeConfigKind  = "EdgeConfig"
)

// resolveEdgeConfig computes the effective EdgeConfigSpec for one Gateway (proposal
// 029), via the NATIVE Gateway API parameters chain:
//
//   - GatewayClass.spec.parametersRef → the fleet-default EdgeConfig (chart-shipped).
//   - Gateway.spec.infrastructure.parametersRef → the per-instance override.
//
// effective = proto.Merge(classDefault, gatewayOverride): the override wins field by
// field; unset fields inherit the class default; anything unset in both falls to the
// compiled best-practice default in ApplyEdgeHardening. Returns nil when neither ref
// resolves to an EdgeConfig (→ pure compiled defaults). Missing/mistyped refs are
// tolerated (logged, skipped) so a dangling parametersRef degrades to defaults rather
// than dropping the Gateway's listeners.
func (r *Reconciler) resolveEdgeConfig(ctx context.Context, gw *gatewayv1.Gateway) *configv1.EdgeConfigSpec {
	var effective *configv1.EdgeConfigSpec

	// Class default.
	if def := r.classDefaultEdgeConfig(ctx); def != nil {
		effective = proto.Clone(def).(*configv1.EdgeConfigSpec)
	}

	// Per-Gateway override (same namespace as the Gateway — LocalParametersReference).
	if inf := gw.Spec.Infrastructure; inf != nil && inf.ParametersRef != nil {
		ref := inf.ParametersRef
		if string(ref.Group) == edgeConfigGroup && string(ref.Kind) == edgeConfigKind {
			if ov := r.getEdgeConfig(ctx, gw.Namespace, string(ref.Name)); ov != nil {
				effective = configapisv1.MergeEdgeConfigSpec(effective, ov)
			}
		}
	}
	return effective
}

// classDefaultEdgeConfig loads the EdgeConfig named by our GatewayClass's
// spec.parametersRef, or nil.
func (r *Reconciler) classDefaultEdgeConfig(ctx context.Context) *configv1.EdgeConfigSpec {
	gc := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: r.GatewayClassName}, gc); err != nil {
		return nil
	}
	ref := gc.Spec.ParametersRef
	if ref == nil || string(ref.Group) != edgeConfigGroup || string(ref.Kind) != edgeConfigKind {
		return nil
	}
	ns := ""
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	return r.getEdgeConfig(ctx, ns, ref.Name)
}

// invalidParametersRef reports whether the Gateway carries an
// infrastructure.parametersRef that cannot resolve: an unsupported group/kind,
// or a named EdgeConfig that doesn't exist. Per the Gateway API spec such a
// Gateway is rejected (Accepted=False/InvalidParameters); absence of a ref is
// fine (compiled defaults).
func (r *Reconciler) invalidParametersRef(ctx context.Context, gw *gatewayv1.Gateway) (string, bool) {
	inf := gw.Spec.Infrastructure
	if inf == nil || inf.ParametersRef == nil {
		return "", false
	}
	ref := inf.ParametersRef
	if string(ref.Group) != edgeConfigGroup || string(ref.Kind) != edgeConfigKind {
		return fmt.Sprintf("unsupported parametersRef %s/%s %q (only %s/%s is supported)", ref.Group, ref.Kind, ref.Name, edgeConfigGroup, edgeConfigKind), true
	}
	if r.getEdgeConfig(ctx, gw.Namespace, string(ref.Name)) == nil {
		return fmt.Sprintf("parametersRef EdgeConfig %s/%s not found", gw.Namespace, ref.Name), true
	}
	return "", false
}

// getEdgeConfig fetches an EdgeConfig CR and returns its proto spec, or nil.
func (r *Reconciler) getEdgeConfig(ctx context.Context, namespace, name string) *configv1.EdgeConfigSpec {
	ec := &configapisv1.EdgeConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, ec); err != nil {
		return nil
	}
	return ec.Spec
}
