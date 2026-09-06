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
//   - dnsConfig timeout/attempts: bounds what a single LOST DNS datagram costs.
//     The resolver a managed pod talks to is node-local (the CNI DNATs :53 to the
//     node's mesh-DNS), so a query that gets no answer is not a slow answer, it is
//     a dropped one — and the stock resolv.conf default makes the client wait a
//     full 5s before retransmitting. Because Go's resolver singleflights by name,
//     that one drop stalls EVERY concurrent lookup of that name for the whole 5s,
//     turning a lost packet into a seconds-long per-name outage (issue #726: a
//     mesh-DNS surge handoff closes the predecessor's SO_REUSEPORT socket, the
//     datagram in flight to it is discarded, and the client eats 5s). A 1s
//     retransmit against a node-local resolver is generous, and 3 attempts leave a
//     3s worst case against the 10s the defaults allow.
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

	aetherlabels "aethermesh.dev/common/constants/labels"
	commonlog "aethermesh.dev/common/log"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	ndotsOption    = "ndots"
	timeoutOption  = "timeout"
	attemptsOption = "attempts"

	// resolverTimeout and resolverAttempts are the resolv.conf retransmit budget
	// injected into managed pods. They are derived constants rather than flags for
	// the same reason ndots is (see podNDots): the value follows from the topology —
	// the resolver is node-local — not from an operator preference.
	resolverTimeout  = "1"
	resolverAttempts = "3"
)

// Mutator injects the mesh resolv.conf options into a pod on CREATE: ndots=NDots
// plus the retransmit budget (timeout/attempts). NDots is the dot count of
// <svc>.<meshDomain> (= the label count of meshDomain; 2 for aether.internal), so
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
// aether.io/managed=false — and (2) injects the mesh resolv.conf options into
// managed pods. A pod that opts out is left entirely untouched. Idempotent:
// re-admission (or a pod that already carries the label / the options) produces no
// spurious patch.
func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Explicit opt-out: a pod in a managed namespace can exclude itself.
	if v, ok := pod.Labels[aetherlabels.LabelAetherManaged]; ok && v != "true" {
		return admission.Allowed("pod opted out of mesh management (aether.io/managed!=true)")
	}

	changed := false
	if pod.Labels[aetherlabels.LabelAetherManaged] != "true" {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[aetherlabels.LabelAetherManaged] = "true"
		changed = true
	}

	for _, opt := range m.dnsOptions() {
		if setDNSOption(pod, opt.name, opt.value) {
			changed = true
		}
	}

	if !changed {
		return admission.Allowed("pod already managed with mesh DNS options")
	}

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	m.Log.DebugContext(ctx, "mesh-injected pod (managed label + DNS options)", "namespace", req.Namespace,
		"ndots", m.NDots, "timeout", resolverTimeout, "attempts", resolverAttempts)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// dnsOption is one resolv.conf option the webhook injects.
type dnsOption struct{ name, value string }

// dnsOptions is the ordered set of resolv.conf options injected into a managed pod.
// An empty value disables that option (NDots is empty when mesh DNS is off); the
// retransmit budget is unconditional because it is a pure robustness bound that
// costs nothing when resolution is healthy.
func (m *Mutator) dnsOptions() []dnsOption {
	return []dnsOption{
		{ndotsOption, m.NDots},
		{timeoutOption, resolverTimeout},
		{attemptsOption, resolverAttempts},
	}
}

// setDNSOption appends name=value to the pod's dnsConfig and reports whether it
// changed the pod. It NEVER overrides an option the pod already carries: an explicit
// value is the workload author's decision, and silently rewriting it would be a
// surprising failure mode to debug. An empty value is a no-op (option disabled).
func setDNSOption(pod *corev1.Pod, name, value string) bool {
	if value == "" || hasDNSOption(pod, name) {
		return false
	}
	if pod.Spec.DNSConfig == nil {
		pod.Spec.DNSConfig = &corev1.PodDNSConfig{}
	}
	v := value
	pod.Spec.DNSConfig.Options = append(pod.Spec.DNSConfig.Options, corev1.PodDNSConfigOption{Name: name, Value: &v})
	return true
}

// hasDNSOption reports whether the pod already declares the named resolv.conf option.
func hasDNSOption(pod *corev1.Pod, name string) bool {
	if pod.Spec.DNSConfig == nil {
		return false
	}
	for _, o := range pod.Spec.DNSConfig.Options {
		if o.Name == name {
			return true
		}
	}
	return false
}
