// Package podmutate contains the controller's pod-mutating admission webhook: it
// injects a low dnsConfig `ndots` into managed pods so a mesh FQDN
// (<svc>.<meshDomain>, e.g. 2 dots) is tried as an absolute name BEFORE the
// cluster.local search list. Without it, the k8s default ndots:5 makes the resolver
// apply the search domains first; glibc tolerates the fall-through to the bare name,
// but musl (Alpine) trips on the churn and fails to resolve mesh names. Mirrors how
// Istio's sidecar injector sets dnsConfig. Default off; opt-in alongside mesh DNS.
package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

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

// Handle injects the ndots option unless the pod already sets one. The webhook's
// objectSelector scopes it to managed pods; this stays a no-op otherwise.
func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if hasNdots(pod) {
		return admission.Allowed("pod already sets dnsConfig ndots")
	}

	if pod.Spec.DNSConfig == nil {
		pod.Spec.DNSConfig = &corev1.PodDNSConfig{}
	}
	v := m.NDots
	pod.Spec.DNSConfig.Options = append(pod.Spec.DNSConfig.Options, corev1.PodDNSConfigOption{Name: ndotsOption, Value: &v})

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	m.Log.DebugContext(ctx, "injected dnsConfig ndots into managed pod", "namespace", req.Namespace, "ndots", v)
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
