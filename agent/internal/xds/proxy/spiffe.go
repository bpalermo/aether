package proxy

import (
	"errors"
	"fmt"

	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

// ErrNoTrustDomain is returned by every builder that would otherwise have to
// format a mesh identity out of an empty trust domain.
//
// The trust domain is LATE-BOUND (agent/internal/identity.TrustDomain, #740),
// so "not known yet" is a real state on the startup path. Emitting anyway
// produces `spiffe:///ns/<ns>/sa/<sa>`: a syntactically valid, semantically
// dead SDS resource name that the agent never serves and Envoy therefore never
// resolves — the inbound listener comes up with NO certificate, every mesh
// connection to the pod fails, and nothing repairs it because the malformed
// name is already published. That is the main-worker-03 outage of 2026-09-19
// (issue #815). Refusing is always better: the caller keeps the config it has
// and retries on the next rebuild.
var ErrNoTrustDomain = errors.New("no SPIFFE trust domain known yet: refusing to build a mesh identity")

// ValidationContextName is the SDS name of the trust bundle for a trust domain
// ("spiffe://<trust-domain>"), or "" when the trust domain is unknown — never
// the bare "spiffe://" that formatting an empty domain would yield.
func ValidationContextName(trustDomain string) string {
	if trustDomain == "" {
		return ""
	}
	return "spiffe://" + trustDomain
}

// SpiffeIDFromPod returns the mesh SPIFFE ID of a local pod, derived — and only
// ever derived — from the mesh trust domain and the pod's own namespace and
// ServiceAccount, following the SPIRE convention
// spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>. It matches the
// identity SPIRE issues for the pod's k8s selectors, so the SDS secret it names
// is the one the SPIRE bridge serves for that pod.
//
// This value is trusted input to the data plane: it names the SDS secret the
// pod's inbound listener presents, the client certificate the pod's egress
// presents (the per-netns transport-socket matcher), and the identity stamped
// into the aether.source authz metadata. It is therefore derived from
// API-server facts only.
//
// It used to honour the aether.io/spiffe-id pod annotation as an override
// (#669). Annotations reach the agent verbatim over the CNI ADD path, so any
// principal able to create a pod could choose the identity that pod's proxy
// config presents — and because the bridge's SDS secrets are node-wide, a pod
// naming a co-located workload's SPIFFE ID had that workload's SVID presented
// on its behalf. The override is gone; SpiffeIDOverrideAnnotation reports the
// annotation so its presence can be logged and counted.
//
// It returns "" — never "spiffe:///ns/…" — when the trust domain is not known
// yet. See ErrNoTrustDomain: every builder that needs a real identity refuses
// on the empty string, so the malformed form is unrepresentable rather than
// merely discouraged.
func SpiffeIDFromPod(cniPod *cniv1.CNIPod, trustDomain string) string {
	if trustDomain == "" || cniPod == nil {
		return ""
	}
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, cniPod.GetNamespace(), cniPod.GetServiceAccount())
}

// SourceIdentityForPod returns the SPIFFE ID that the pod's mesh-originating
// listener chains stamp into filter state (sourceIdentityFilterStateKey,
// networkfilter.go) so the cluster transport-socket matcher can select the
// pod's client certificate by identity rather than by netns path (issue #815).
//
// An unknown trust domain yields "" (SpiffeIDFromPod), and the chains then
// carry only the netns key — byte-for-byte the pre-#815 shape — picking the
// identity up on the next rebuild once the trust domain is known. The trust
// domain is late-bound (agent/internal/identity), so this is a real, if brief,
// startup state. Unlike the inbound SERVER certificate, a missing source
// filter-state value is not fatal in release one: nothing reads the key yet
// (the cluster matcher still keys on the netns until release two).
func SourceIdentityForPod(cniPod *cniv1.CNIPod, trustDomain string) string {
	return SpiffeIDFromPod(cniPod, trustDomain)
}

// SpiffeIDOverrideAnnotation returns the pod's aether.io/spiffe-id annotation
// value and whether a non-empty one is present. The value is never honoured
// (see SpiffeIDFromPod); callers use it to surface the rejected override — WARN
// log plus a counter — so an attempted identity override is never silent.
func SpiffeIDOverrideAnnotation(cniPod *cniv1.CNIPod) (string, bool) {
	id := cniPod.GetAnnotations()[xdsconst.AnnotationSpiffeID]
	return id, id != ""
}
