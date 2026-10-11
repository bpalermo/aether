package gatewayapi

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Gateway infrastructure metadata (Gateway API spec.infrastructure.labels and
// .annotations, GEP-1762).
//
// The one resource the edge generates for a Gateway is its LoadBalancer Service
// in the edge namespace (proposal 021 Phase 2); the pods and the ServiceAccount
// belong to the shared edge Deployment and serve every Gateway, so a Gateway's
// metadata has no business on them. The Service is therefore the only thing the
// labels and annotations are copied to.
//
// The Service is written by others too (a load-balancer controller annotates it,
// a person may label it), and the edge writes it with a read-modify-update, not
// with server-side apply, so there is no field manager to say which keys came
// from the Gateway. AnnotationInfrastructureKeys is that record: the keys the
// edge copied last time. A key in the record that the Gateway no longer sets is
// removed; a key that is not in the record is never removed.

// AnnotationInfrastructureKeys is the annotation on a per-Gateway LoadBalancer
// Service that records which label and annotation keys the edge copied onto it
// from the Gateway's spec.infrastructure. Its value is a JSON object,
// {"labels":[...],"annotations":[...]}, keys sorted. It is absent when the
// Gateway sets none.
const AnnotationInfrastructureKeys = "aether.io/infrastructure-keys"

// reservedInfrastructureDomains are the key prefixes (the DNS subdomain before
// the "/") a Gateway cannot set through spec.infrastructure: aether's own
// (LabelEdgeGateway decides which Services are garbage-collected, and the record
// above decides what is removed) and the Gateway API's (its labels state facts
// about generated resources that the edge would be forging).
var reservedInfrastructureDomains = []string{"aether.io", "gateway.networking.k8s.io"}

// reservedInfrastructureKey reports why key cannot be set from a Gateway's
// spec.infrastructure, or "" when it can.
func reservedInfrastructureKey(key string) string {
	if key == AnnotationMetalLBLoadBalancerIPs {
		return "the address pin is set from spec.addresses"
	}
	domain, _, prefixed := strings.Cut(key, "/")
	if !prefixed {
		return ""
	}
	for _, d := range reservedInfrastructureDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return "the " + d + " prefix is reserved"
		}
	}
	return ""
}

// infrastructureMetadata is what a Gateway's spec.infrastructure puts on the
// Service generated for it: the labels and annotations that are applied, and a
// description of each key that is not ("label <key> (<why>)"), sorted.
type infrastructureMetadata struct {
	labels      map[string]string
	annotations map[string]string
	refused     []string
}

// gatewayInfrastructureMetadata reads a Gateway's spec.infrastructure labels and
// annotations and sorts them into applied and refused. A key is refused when it
// is reserved (reservedInfrastructureKey) or when the API server would reject it:
// one invalid label fails the whole Service write, and with it the Gateway's
// address. The Gateway CRD validates keys and label values already, so the
// second case needs validation to be off or an older CRD.
func gatewayInfrastructureMetadata(gw *gatewayv1.Gateway) infrastructureMetadata {
	var m infrastructureMetadata
	inf := gw.Spec.Infrastructure
	if inf == nil {
		return m
	}
	refuse := func(kind, key, why string) {
		m.refused = append(m.refused, kind+" "+key+" ("+why+")")
	}
	for k, v := range inf.Labels {
		key, value := string(k), string(v)
		switch {
		case reservedInfrastructureKey(key) != "":
			refuse("label", key, reservedInfrastructureKey(key))
		case len(validation.IsQualifiedName(key)) > 0:
			refuse("label", key, "not a valid label key")
		case len(validation.IsValidLabelValue(value)) > 0:
			refuse("label", key, "not a valid label value")
		default:
			if m.labels == nil {
				m.labels = map[string]string{}
			}
			m.labels[key] = value
		}
	}
	for k, v := range inf.Annotations {
		key := string(k)
		switch {
		case reservedInfrastructureKey(key) != "":
			refuse("annotation", key, reservedInfrastructureKey(key))
		case len(validation.IsQualifiedName(key)) > 0:
			refuse("annotation", key, "not a valid annotation key")
		default:
			if m.annotations == nil {
				m.annotations = map[string]string{}
			}
			m.annotations[key] = string(v)
		}
	}
	slices.Sort(m.refused)
	return m
}

// infrastructureKeyRecord is the value of AnnotationInfrastructureKeys.
type infrastructureKeyRecord struct {
	Labels      []string `json:"labels,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
}

// readInfrastructureKeyRecord returns the keys the edge recorded on s. The
// annotation sits on an object others can write, so it is read as data: a value
// that does not parse records nothing (nothing is removed), and a reserved key
// in it is dropped, so no record can make the edge remove its own label, the
// address pin or the record itself.
func readInfrastructureKeyRecord(s *corev1.Service) infrastructureKeyRecord {
	var rec infrastructureKeyRecord
	raw, ok := s.Annotations[AnnotationInfrastructureKeys]
	if !ok {
		return rec
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return infrastructureKeyRecord{}
	}
	reserved := func(k string) bool { return reservedInfrastructureKey(k) != "" }
	rec.Labels = slices.DeleteFunc(rec.Labels, reserved)
	rec.Annotations = slices.DeleteFunc(rec.Annotations, reserved)
	return rec
}

// applyTo makes s carry the Gateway's labels and annotations: it removes the
// keys the record says the edge set earlier and the Gateway no longer sets, sets
// the current ones (the Gateway wins over a value someone else put under the
// same key, and the key is the Gateway's from then on), and rewrites the record.
// Every other label and annotation on s is left alone. A Gateway that sets
// nothing, on a Service with no record, leaves s untouched.
func (m infrastructureMetadata) applyTo(s *corev1.Service) {
	prev := readInfrastructureKeyRecord(s)
	for _, k := range prev.Labels {
		if _, keep := m.labels[k]; !keep {
			delete(s.Labels, k)
		}
	}
	for _, k := range prev.Annotations {
		if _, keep := m.annotations[k]; !keep {
			delete(s.Annotations, k)
		}
	}
	if len(m.labels) > 0 && s.Labels == nil {
		s.Labels = map[string]string{}
	}
	maps.Copy(s.Labels, m.labels)

	if len(m.labels) == 0 && len(m.annotations) == 0 {
		delete(s.Annotations, AnnotationInfrastructureKeys)
		return
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	maps.Copy(s.Annotations, m.annotations)
	rec := infrastructureKeyRecord{
		Labels:      slices.Sorted(maps.Keys(m.labels)),
		Annotations: slices.Sorted(maps.Keys(m.annotations)),
	}
	// Two slices of strings always marshal.
	raw, _ := json.Marshal(rec)
	s.Annotations[AnnotationInfrastructureKeys] = string(raw)
}

// refusedMessage is the sentence appended to the Gateway's Accepted condition
// when keys were refused, or "". The Gateway stays accepted: the Gateway API
// defines no condition reason for ignored metadata, and a Gateway that routes
// must not be rejected over a label.
func (m infrastructureMetadata) refusedMessage() string {
	if len(m.refused) == 0 {
		return ""
	}
	return "spec.infrastructure keys ignored: " + strings.Join(m.refused, ", ")
}

// logRefusedInfrastructureKeys logs a Gateway's refused spec.infrastructure keys
// when they change, not on every pass: the reconciler re-projects everything on
// any watched change.
func (r *Reconciler) logRefusedInfrastructureKeys(ctx context.Context, gk gatewayKey, m infrastructureMetadata) {
	msg := strings.Join(m.refused, ", ")
	r.infraRefusedMu.Lock()
	prev := r.infraRefused[gk]
	if msg == "" {
		delete(r.infraRefused, gk)
	} else {
		if r.infraRefused == nil {
			r.infraRefused = map[gatewayKey]string{}
		}
		r.infraRefused[gk] = msg
	}
	r.infraRefusedMu.Unlock()
	if msg == "" || msg == prev {
		return
	}
	r.Log.WarnContext(ctx, "spec.infrastructure keys ignored on the per-Gateway Service",
		"gateway", gk.Namespace+"/"+gk.Name, "ignored", msg)
}

// forgetRefusedInfrastructureKeys drops the logging state of Gateways that are
// gone, so the map does not grow with every Gateway the edge has ever seen.
func (r *Reconciler) forgetRefusedInfrastructureKeys(current []gatewayv1.Gateway) {
	r.infraRefusedMu.Lock()
	defer r.infraRefusedMu.Unlock()
	maps.DeleteFunc(r.infraRefused, func(gk gatewayKey, _ string) bool {
		return !slices.ContainsFunc(current, func(gw gatewayv1.Gateway) bool {
			return gw.Namespace == gk.Namespace && gw.Name == gk.Name
		})
	})
}
