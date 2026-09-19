package proxy

import (
	"fmt"

	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

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
func SpiffeIDFromPod(cniPod *cniv1.CNIPod, trustDomain string) string {
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, cniPod.GetNamespace(), cniPod.GetServiceAccount())
}

// SourceIdentityForPod returns the SPIFFE ID that the pod's mesh-originating
// listener chains stamp into filter state (sourceIdentityFilterStateKey,
// networkfilter.go) so the cluster transport-socket matcher can select the
// pod's client certificate by identity rather than by netns path (issue #815).
//
// It is SpiffeIDFromPod with one guard: an unknown trust domain yields "", not
// the malformed "spiffe:///ns/…/sa/…" that formatting an empty domain would
// produce. The chains then carry only the netns key — byte-for-byte the
// pre-#815 shape — and pick the identity up on the next rebuild once the trust
// domain is known. The trust domain is late-bound (agent/internal/identity), so
// this is a real, if brief, startup state.
func SourceIdentityForPod(cniPod *cniv1.CNIPod, trustDomain string) string {
	if cniPod == nil || trustDomain == "" {
		return ""
	}
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
