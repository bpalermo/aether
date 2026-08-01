// Package gatewaystatus contains shared helpers for writing Gateway API status:
// controller-owned RouteParentStatus entries on Routes and conditions on
// Gateways/GatewayClasses. The merge rules follow the Gateway API spec: an
// implementation MUST only update entries (RouteParentStatus / conditions) that
// carry its own controllerName, and MUST NOT remove or reorder entries owned by
// other controllers.
//
// This is the conformance on-ramp: the upstream suite asserts Accepted/
// ResolvedRefs on routes and Accepted/Programmed on Gateways for nearly every
// test, so every reconciler that owns a parent must publish these conditions.
package gatewaystatus

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// EdgeControllerName is the controllerName of the edge GatewayClass
	// (charts/aether/templates/edge-gatewayclass.yaml). The edge reconciler owns
	// the RouteParentStatus for Gateway-parented routes and the Gateway/
	// GatewayClass status carrying this name.
	EdgeControllerName gatewayv1.GatewayController = "gateway.aether.io/edge"

	// MeshControllerName is the controllerName the mesh (GAMMA + l4route)
	// reconcilers use when writing the RouteParentStatus for Service-parented
	// (east-west) routes. There is no GatewayClass for the mesh — a Service is
	// the parent — but the controllerName still scopes our owned status entry.
	MeshControllerName gatewayv1.GatewayController = "gateway.aether.io/mesh"
)

// Condition is a minimal description of a status condition to set: its type,
// status, reason, and a human message. ObservedGeneration is filled in by the
// merge helpers from the owning object's generation.
type Condition struct {
	Type    string
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// MergeRouteParentStatus upserts the controller-owned RouteParentStatus for the
// given parentRef into parents, setting the supplied conditions (with
// observedGeneration). Entries owned by other controllers (different
// controllerName) are preserved untouched, and an existing matching entry is
// updated in place. It returns the new parents slice and whether it differs from
// the input — callers should only issue a status update when changed is true to
// avoid hot reconcile loops.
func MergeRouteParentStatus(
	parents []gatewayv1.RouteParentStatus,
	controllerName gatewayv1.GatewayController,
	parentRef gatewayv1.ParentReference,
	observedGeneration int64,
	conditions ...Condition,
) (result []gatewayv1.RouteParentStatus, changed bool) {
	// Find an existing entry matching BOTH our controllerName and this parentRef.
	idx := -1
	for i := range parents {
		if parents[i].ControllerName != controllerName {
			continue
		}
		if parentRefEqual(parents[i].ParentRef, parentRef) {
			idx = i
			break
		}
	}

	var existing gatewayv1.RouteParentStatus
	if idx >= 0 {
		existing = *parents[idx].DeepCopy()
	} else {
		existing = gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: controllerName,
		}
	}
	for _, c := range conditions {
		meta.SetStatusCondition(&existing.Conditions, metav1.Condition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			ObservedGeneration: observedGeneration,
		})
	}

	if idx >= 0 {
		if routeParentStatusEqual(parents[idx], existing) {
			return parents, false
		}
		result = make([]gatewayv1.RouteParentStatus, len(parents))
		copy(result, parents)
		result[idx] = existing
		return result, true
	}
	result = make([]gatewayv1.RouteParentStatus, 0, len(parents)+1)
	result = append(result, parents...)
	result = append(result, existing)
	return result, true
}

// MergeConditions sets the supplied conditions (with observedGeneration) into an
// existing condition slice using meta.SetStatusCondition, returning the new
// slice and whether anything changed. Conditions owned by other actors (other
// Types) are preserved. Used for Gateway, GatewayClass, and Listener conditions.
func MergeConditions(
	current []metav1.Condition,
	observedGeneration int64,
	conditions ...Condition,
) (result []metav1.Condition, changed bool) {
	result = make([]metav1.Condition, len(current))
	copy(result, current)
	for _, c := range conditions {
		// meta.SetStatusCondition mutates the matching element in place and returns
		// whether anything changed; use that return directly. The previous
		// before/after comparison captured `before` as a pointer INTO result, which
		// SetStatusCondition then mutates in place — so before and after aliased the
		// same (already-updated) element and a transition on a PRE-EXISTING condition
		// (e.g. a GatewayClass that ships a default Accepted=Unknown/Pending
		// condition) was misdetected as no-change, and the status was never written.
		if meta.SetStatusCondition(&result, metav1.Condition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			ObservedGeneration: observedGeneration,
		}) {
			changed = true
		}
	}
	return result, changed
}

// parentRefEqual reports whether two parentRefs identify the same parent. We
// always write back the same parentRef we read from the spec, so comparing the
// dereferenced fields (with their zero-value defaults) suffices.
func parentRefEqual(a, b gatewayv1.ParentReference) bool {
	return derefGroup(a.Group) == derefGroup(b.Group) &&
		derefKind(a.Kind) == derefKind(b.Kind) &&
		derefNamespace(a.Namespace) == derefNamespace(b.Namespace) &&
		a.Name == b.Name &&
		derefSectionName(a.SectionName) == derefSectionName(b.SectionName) &&
		derefPort(a.Port) == derefPort(b.Port)
}

func derefGroup(g *gatewayv1.Group) gatewayv1.Group {
	if g == nil {
		return ""
	}
	return *g
}

func derefKind(k *gatewayv1.Kind) gatewayv1.Kind {
	if k == nil {
		return ""
	}
	return *k
}

func derefNamespace(n *gatewayv1.Namespace) gatewayv1.Namespace {
	if n == nil {
		return ""
	}
	return *n
}

func derefSectionName(s *gatewayv1.SectionName) gatewayv1.SectionName {
	if s == nil {
		return ""
	}
	return *s
}

func derefPort(p *gatewayv1.PortNumber) gatewayv1.PortNumber {
	if p == nil {
		return 0
	}
	return *p
}

func routeParentStatusEqual(a, b gatewayv1.RouteParentStatus) bool {
	if a.ControllerName != b.ControllerName || !parentRefEqual(a.ParentRef, b.ParentRef) {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		if !conditionSemanticEqual(a.Conditions[i], b.Conditions[i]) {
			return false
		}
	}
	return true
}

// conditionSemanticEqual compares two conditions ignoring LastTransitionTime
// (meta.SetStatusCondition only bumps that when Status flips, so comparing it
// would defeat the no-op detection).
func conditionSemanticEqual(a, b metav1.Condition) bool {
	return a.Type == b.Type &&
		a.Status == b.Status &&
		a.Reason == b.Reason &&
		a.Message == b.Message &&
		a.ObservedGeneration == b.ObservedGeneration
}
