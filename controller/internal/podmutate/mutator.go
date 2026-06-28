// Package podmutate contains the controller's pod-mutating admission webhook. It
// does two things on pod CREATE, mirroring Istio's sidecar injector:
//
//   - Namespace auto-injection: a pod created in a namespace labeled
//     aether.io/managed=true is given the aether.io/managed=true POD label so the
//     CNI meshes it — no per-pod label needed. A pod that explicitly sets
//     aether.io/managed=false opts OUT (left unmanaged), so individual workloads
//     (Jobs, the prober, infra) can be excluded from an otherwise-managed namespace.
//   - dnsConfig ndots: injects a low ndots into managed pods so a mesh FQDN
//     (<svc>.<meshDomain>, e.g. 2 dots) is tried as an absolute name BEFORE the
//     cluster.local search list. Without it the k8s default ndots:5 makes the
//     resolver apply the search domains first; glibc tolerates the fall-through to
//     the bare name, but musl (Alpine) trips on the churn and fails to resolve mesh
//     names. ndots is opt-in (default off) alongside mesh DNS.
//
// The webhook is wired with two rules: an objectSelector (aether.io/managed=true
// pods, in any namespace) and a namespaceSelector (pods in aether.io/managed=true
// namespaces). Both dispatch here; Handle is idempotent for either entry point.
package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/bpalermo/aether/common/constants"
	commonlog "github.com/bpalermo/aether/common/log"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const ndotsOption = "ndots"

// Mutator injects dnsConfig ndots=NDots into a pod on CREATE. NDots is the dot count
// of <svc>.<meshDomain> (= the label count of meshDomain; 2 for aether.internal), so
// mesh FQDNs are resolved absolute-first while shorter k8s names keep their search
// behavior.
type Mutator struct {
	NDots string
	Log   *slog.Logger
}

// NewMutator builds the pod-ndots mutator.
func NewMutator(ndots string, log *slog.Logger) *Mutator {
	return &Mutator{NDots: ndots, Log: commonlog.Named(log, "pod-ndots")}
}

// Handle reaches here for a pod matched either by the managed-pod objectSelector
// or the managed-namespace namespaceSelector. It (1) ensures the aether.io/managed
// label so the CNI meshes the pod — unless the pod explicitly opts out with
// aether.io/managed=false — and (2) injects ndots into managed pods. A pod that
// opts out is left entirely untouched. Idempotent: re-admission (or a pod that
// already carries the label / ndots) produces no spurious patch.
func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Explicit opt-out: a pod in a managed namespace can exclude itself.
	if v, ok := pod.Labels[constants.LabelAetherManaged]; ok && v != "true" {
		return admission.Allowed("pod opted out of mesh management (aether.io/managed!=true)")
	}

	changed := false
	if pod.Labels[constants.LabelAetherManaged] != "true" {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[constants.LabelAetherManaged] = "true"
		changed = true
	}

	if m.NDots != "" && !hasNdots(pod) {
		if pod.Spec.DNSConfig == nil {
			pod.Spec.DNSConfig = &corev1.PodDNSConfig{}
		}
		v := m.NDots
		pod.Spec.DNSConfig.Options = append(pod.Spec.DNSConfig.Options, corev1.PodDNSConfigOption{Name: ndotsOption, Value: &v})
		changed = true
	}

	if !changed {
		return admission.Allowed("pod already managed with ndots")
	}

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	m.Log.DebugContext(ctx, "mesh-injected pod (managed label + ndots)", "namespace", req.Namespace, "ndots", m.NDots)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

func hasNdots(pod *corev1.Pod) bool {
	if pod.Spec.DNSConfig == nil {
		return false
	}
	for _, o := range pod.Spec.DNSConfig.Options {
		if o.Name == ndotsOption {
			return true
		}
	}
	return false
}
