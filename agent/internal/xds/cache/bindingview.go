package cache

import (
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

// bindingView is what the two identity-binding logs (#638) need besides the
// resources of the snapshot they describe: which pod each per-pod listener
// belongs to, and the netns → identity index.
//
// The logs used to read the cache's maps when they ran, after SetSnapshot, so
// a line could name a pod or a cluster that a mutator added after the build
// read its resources, under the version of a snapshot that does not carry it
// (#1621). They are now made from the snapshot's own resources: the listeners
// and the secrets the build put in it, and the mTLS-injected clusters it
// published from its read of the cluster map (pinReport.mtls). This view supplies the rest, and it is
// joined to the snapshot by identity: a pod is named only when the very
// listener proto its entry held when the view was taken is one of the
// snapshot's listeners (publishedListeners).
//
// The view and the listener set are two reads of the listener map
// (SnapshotCache.Listeners takes its own lock), a few statements apart in the
// same build. What that leaves:
//
//   - An entry written between the two reads is in the snapshot and not in
//     the view, so this build names nothing of it. The next build does, under
//     its own version (AddPod builds one right after its write).
//   - An entry removed, or replaced, between the two reads is in the view and
//     its listeners are not in the snapshot: it is not named. A replaced one
//     that an earlier build had named is thereby dropped from the table the
//     lines are diffed against, so the next build names it again although
//     its binding did not change.
//   - The netns → identity index is no resource of a snapshot. It is read
//     here, with the view, and not when the snapshot is set.
type bindingView struct {
	// trustDomain is the trust domain in force when the view was taken; ""
	// when none is known yet.
	trustDomain string
	// workloads is a copy of the netns → SPIFFE ID index (c.localWorkloads).
	// Nil while the node SVID is unserved: no upstream mTLS is injected then,
	// so no cluster binds a client certificate.
	workloads map[string]string
	// pods is one item per listener entry.
	pods []podBindingView
}

// podBindingView is one listener entry: its pod, and the listeners the
// bindings are read from or joined by.
type podBindingView struct {
	netns string
	pod   *cniv1.CNIPod
	// inbound carries the pod's server certificate; outbound and capture are
	// the listeners that stamp its source identity on egress.
	inbound, outbound, capture types.Resource
}

// takeBindingView reads the view. The three reads take their own locks, one
// after the other and never nested, so this adds no lock-ordering constraint.
// Called by generateSnapshot (snapshotMu held) right before it reads the
// listener set.
func (c *SnapshotCache) takeBindingView() bindingView {
	v := bindingView{trustDomain: c.currentTrustDomain()}

	c.localMu.RLock()
	if c.nodeSpiffeID != "" && len(c.localWorkloads) > 0 {
		v.workloads = make(map[string]string, len(c.localWorkloads))
		for netns, id := range c.localWorkloads {
			v.workloads[netns] = id
		}
	}
	c.localMu.RUnlock()

	c.listenerMu.RLock()
	if len(c.listeners) > 0 {
		v.pods = make([]podBindingView, 0, len(c.listeners))
		for netns, entry := range c.listeners {
			v.pods = append(v.pods, podBindingView{
				netns: netns, pod: entry.cniPod,
				inbound: entry.inbound, outbound: entry.outbound, capture: entry.capture,
			})
		}
	}
	c.listenerMu.RUnlock()
	return v
}

// publishedListeners is the set of the listener resources a snapshot carries,
// by identity. Nil when the view has no pod to join to them.
func (v bindingView) publishedListeners(listeners []types.Resource) map[types.Resource]struct{} {
	if len(v.pods) == 0 {
		return nil
	}
	published := make(map[types.Resource]struct{}, len(listeners))
	for _, l := range listeners {
		published[l] = struct{}{}
	}
	return published
}

// sourceBindings joins the netns → identity index with the pod that owns each
// netns: what each local source pod's egress presents in the snapshot whose
// listeners are `published`. Nil while the node SVID is unserved or no
// identity is indexed.
//
// A source whose pod is known and whose egress listeners are not in the
// snapshot is left out: the snapshot delivers no binding for it (a pod whose
// netns is gone, whose listeners the build skips; a pod removed or rebuilt
// since the view was taken). An identity indexed under a netns no entry owns
// stays in, with no pod: that orphan is what reportBindingMismatches warns
// about.
func (v bindingView) sourceBindings(published map[types.Resource]struct{}) map[string]sourceBinding {
	if len(v.workloads) == 0 {
		return nil
	}
	sources := make(map[string]sourceBinding, len(v.workloads))
	for netns, id := range v.workloads {
		sources[netns] = sourceBinding{presented: id}
	}
	for _, p := range v.pods {
		b, ok := sources[p.netns]
		if !ok || p.pod == nil {
			continue
		}
		_, outbound := published[p.outbound]
		_, capture := published[p.capture]
		if !outbound && !capture {
			delete(sources, p.netns)
			continue
		}
		b.pod = p.pod.GetNamespace() + "/" + p.pod.GetName()
		b.podIdentity = proxy.SpiffeIDFromPod(p.pod, v.trustDomain)
		sources[p.netns] = b
	}
	return sources
}

// inboundBindings names, per filter chain of every inbound listener the
// snapshot carries, the server certificate it binds. secrets is the snapshot's
// own secret set: a chain is served when the snapshot that carries it also
// carries the secret it names. Nil before a trust domain is known (nothing can
// be compared then). Cleartext chains (SPIRE off) carry no transport socket
// and are skipped: they present no certificate at all.
func (v bindingView) inboundBindings(published map[types.Resource]struct{}, secrets []types.Resource) map[string]inboundBinding {
	if v.trustDomain == "" || len(v.pods) == 0 {
		return nil
	}
	served := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		served[cachev3.GetResourceName(s)] = struct{}{}
	}
	out := make(map[string]inboundBinding, len(v.pods)*inboundChainsPerPod)
	for _, p := range v.pods {
		if _, ok := published[p.inbound]; !ok {
			continue
		}
		collectPodInboundBindings(p.pod, p.inbound, v.trustDomain, served, out)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
