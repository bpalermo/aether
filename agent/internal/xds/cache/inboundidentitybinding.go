package cache

import (
	"context"
	"sort"
	"strings"

	"aethermesh.dev/agent/internal/xds/proxy"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
)

// The INBOUND identity-binding discriminator (issue #638).
//
// WHY THIS EXISTS, AND WHY IT IS NOT #686. Envoy's SAN-matcher failure message
// (source/common/tls/cert_validator/default_validator.cc:332)
//
//	verify cert failed: SAN matcher, certificate SANs are [...]
//
// prints the SANs of the certificate being VALIDATED — the one the PEER
// presented. envoy_cluster_ssl_fail_verify_san is therefore a CLIENT-side
// counter about the SERVER's certificate. Every ledger join on #638 read it the
// other way round. Inverted, the wrong identity in a #638 event belongs to a
// SERVER certificate: the inbound filter chain → SDS server-secret binding of
// whichever proxy TERMINATED the connection. For same-node traffic that proxy
// is the restarting one, which is exactly the observed "node-wide constant per
// time slice" shape. #686's outbound check watches the client side and
// structurally cannot fire for this; this file is its inbound counterpart.
//
// WHAT THE INBOUND BINDING IS. proxy.NewInboundListener derives the chain's
// server-certificate SDS name from the SAME *cniv1.CNIPod it names the listener
// and chains after (proxy.SpiffeIDFromPod, ingress.go), so within one build the
// invariant holds BY CONSTRUCTION — a Go-level mismatch cannot be constructed.
// What this check adds is that it does not read that Go string: it reads the
// DownstreamTlsContext out of the listener proto the snapshot just handed
// Envoy, and compares it with the identity of the pod recorded as owning that
// listener entry. That catches what construction cannot rule out:
//
//   - a listener entry rebuilt from, or left behind by, a DIFFERENT pod than the
//     one now owning the netns (netns reuse after a missed CNI DEL — the same
//     substrate #686 documents for the outbound index);
//   - a rebuild path (gamma.go, l4route.go, capture.go, udspolicy.go) that
//     re-generates a pod's listeners from the wrong entry;
//   - a chain whose named secret is NOT in the snapshot's secret set, i.e. the
//     chain reached Envoy before its own SVID did. That is the remaining
//     candidate mechanism for #638: Envoy selecting some other secret (or an
//     older generation of one) while the named one is unserved.
//
// SO THE COUNTER IS A DECISIVE INSTRUMENT EITHER WAY. If
// aether.agent.identity.inbound_binding_mismatch increments during a #638
// event, the mis-binding is in the agent's snapshot and the WARN names the pod,
// both identities and the snapshot version. If it stays 0 across an event while
// the INFO lines show the chains were (re)bound correctly, the agent's snapshot
// was right and the defect is in Envoy's SDS/secret lifecycle across the hot
// restart — which closes the agent-side line of enquiry.
//
// Fail-open throughout: this observes the snapshot after SetSnapshot and never
// blocks, alters or delays it.

// inboundChainsPerPod is a sizing hint only: the TCP floor chain, the no-SNI h2
// chain and one chain per served port.
const inboundChainsPerPod = 4

// inboundBinding is the server certificate one inbound filter chain will
// PRESENT to callers, joined with the identity the pod it serves is entitled to.
type inboundBinding struct {
	// pod is "<namespace>/<name>" of the pod this listener entry belongs to.
	pod string
	// podIdentity is the SPIFFE ID derived from that pod's own namespace and
	// ServiceAccount — the identity its inbound chains MUST present.
	podIdentity string
	// presented is the tls_certificate_sds_secret_config name read back out of
	// the chain's DownstreamTlsContext: the SDS server certificate Envoy will
	// serve on this chain. Should equal podIdentity.
	presented string
	// served reports whether the snapshot that carries this chain also carries
	// a secret of that name. False means the chain reached Envoy ahead of its
	// own SVID.
	served bool
}

// foreign reports whether the chain would present an identity that is not its
// own pod's.
func (b inboundBinding) foreign() bool {
	return b.podIdentity != "" && b.podIdentity != b.presented
}

// inboundBindingState is the previous snapshot's inbound binding table, kept so
// the logging is edge-triggered (steady state is silent).
type inboundBindingState struct {
	// chains is "<listener>/<chain>" → binding.
	chains map[string]inboundBinding
}

// logInboundIdentityBindings emits the (inbound filter chain → SDS server-cert
// secret) bindings this snapshot delivers, but ONLY the ones that changed since
// the previous snapshot (or are seen for the first time), and WARNs about any
// chain whose server certificate is not the identity of the pod it serves.
// Called from generateSnapshot with snapshotMu held, which also serializes the
// stored state.
func (c *SnapshotCache) logInboundIdentityBindings(ctx context.Context, version string) {
	chains := c.collectInboundBindings()

	c.bindingMu.Lock()
	prev := c.lastInboundBindings
	c.lastInboundBindings = inboundBindingState{chains: chains}
	c.bindingMu.Unlock()

	changed := diffInboundBindings(prev.chains, chains)
	if len(changed) == 0 {
		return
	}

	c.reportInboundMismatches(ctx, version, chains, changed)
	c.emitInboundBindingChanges(ctx, version, prev.chains, chains, changed)
}

// collectInboundBindings reads each local pod's inbound listener out of the
// cache and names, per filter chain, the server certificate it binds. Returns
// nil before any listener load has recorded a trust domain (nothing can be
// compared then). Cleartext chains (SPIRE off) carry no transport socket and
// are skipped: they present no certificate at all.
func (c *SnapshotCache) collectInboundBindings() map[string]inboundBinding {
	c.localMu.RLock()
	trustDomain := c.trustDomain
	c.localMu.RUnlock()
	if trustDomain == "" {
		return nil
	}

	served := c.servedSecretNames()

	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()

	out := make(map[string]inboundBinding, len(c.listeners)*inboundChainsPerPod)
	for _, entry := range c.listeners {
		collectPodInboundBindings(entry, trustDomain, served, out)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectPodInboundBindings adds one entry per certificate-bearing inbound
// filter chain of a single pod.
func collectPodInboundBindings(entry listenerEntry, trustDomain string, served map[string]struct{}, out map[string]inboundBinding) {
	l, ok := entry.inbound.(*listenerv3.Listener)
	if !ok || l == nil || entry.cniPod == nil {
		return
	}
	b := inboundBinding{
		pod:         entry.cniPod.GetNamespace() + "/" + entry.cniPod.GetName(),
		podIdentity: proxy.SpiffeIDFromPod(entry.cniPod, trustDomain),
	}
	for _, fc := range l.GetFilterChains() {
		secret := downstreamCertSecretName(fc.GetTransportSocket())
		if secret == "" {
			continue
		}
		b.presented = secret
		_, b.served = served[secret]
		out[l.GetName()+"/"+fc.GetName()] = b
	}
}

// downstreamCertSecretName reads the server certificate's SDS secret name out
// of a filter chain's transport socket, or "" when the chain terminates no TLS
// (cleartext) or the socket is not a DownstreamTlsContext.
func downstreamCertSecretName(ts *corev3.TransportSocket) string {
	tc := ts.GetTypedConfig()
	if tc == nil {
		return ""
	}
	var dtc tlsv3.DownstreamTlsContext
	if err := tc.UnmarshalTo(&dtc); err != nil {
		return ""
	}
	cfgs := dtc.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()
	if len(cfgs) == 0 {
		return ""
	}
	return cfgs[0].GetName()
}

// servedSecretNames snapshots the secret names this node's snapshot carries.
func (c *SnapshotCache) servedSecretNames() map[string]struct{} {
	c.secretMu.RLock()
	defer c.secretMu.RUnlock()

	names := make(map[string]struct{}, len(c.secrets))
	for name := range c.secrets {
		names[name] = struct{}{}
	}
	return names
}

// diffInboundBindings returns the chains whose binding is new or changed,
// sorted for deterministic log order. On the first call prev is nil, so every
// chain is reported as new.
func diffInboundBindings(prev, chains map[string]inboundBinding) []string {
	var changed []string
	for key, b := range chains {
		if old, ok := prev[key]; !ok || old != b {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

// reportInboundMismatches WARNs for every changed chain whose server
// certificate is not its own pod's SPIFFE ID, and counts them. Mismatch status
// is a function of the binding, so a mismatch appearing or clearing always
// shows up as a changed chain — no persistent mismatch is reported twice for
// one state.
func (c *SnapshotCache) reportInboundMismatches(ctx context.Context, version string, chains map[string]inboundBinding, changed []string) {
	var mismatches int64
	for _, key := range changed {
		b := chains[key]
		switch {
		case b.foreign():
			mismatches++
			c.log.WarnContext(ctx, "inbound chain bound to a foreign identity",
				"chain", key,
				"pod", b.pod,
				"pod_spiffe_id", b.podIdentity,
				"bound_spiffe_id", b.presented,
				"secret", b.presented,
				"secret_served", b.served,
				"snapshot_version", version)
		case !b.served:
			// The chain reached Envoy before its own SVID: the window in which
			// Envoy has a filter chain whose named secret it cannot resolve.
			c.log.WarnContext(ctx, "inbound chain references a secret absent from the snapshot",
				"chain", key,
				"pod", b.pod,
				"secret", b.presented,
				"snapshot_version", version)
		}
	}
	c.metrics.InboundBindingMismatch(ctx, mismatches)
}

// emitInboundBindingChanges logs one line per changed chain, naming both the
// identity it now presents and the one it presented before (empty on a first
// bind), so a single line is self-diagnosing.
func (c *SnapshotCache) emitInboundBindingChanges(ctx context.Context, version string, prev, chains map[string]inboundBinding, changed []string) {
	if len(changed) > maxBindingChangeLines {
		c.logInboundBindingSummary(ctx, version, chains, changed)
		return
	}
	for _, key := range changed {
		b := chains[key]
		c.log.InfoContext(ctx, "inbound identity binding",
			"chain", key,
			"pod", b.pod,
			"pod_spiffe_id", b.podIdentity,
			"secret", b.presented,
			"previous_secret", prev[key].presented,
			"secret_served", b.served,
			"snapshot_version", version)
	}
}

// logInboundBindingSummary replaces the per-chain lines when one snapshot
// re-binds more than maxBindingChangeLines chains (a cold start binds every
// chain of every local pod). It keeps the diagnostic part — the distinct
// pod→identity transitions — and drops only the per-chain repetition, which is
// redundant: every chain of a pod binds that pod's one certificate.
func (c *SnapshotCache) logInboundBindingSummary(ctx context.Context, version string, chains map[string]inboundBinding, changed []string) {
	seen := make(map[string]struct{}, len(changed))
	transitions := make([]string, 0, maxSummaryTransitions+1)
	for _, key := range changed {
		b := chains[key]
		t := b.pod + "=" + b.presented
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		if len(transitions) == maxSummaryTransitions {
			transitions = append(transitions, "...")
			break
		}
		transitions = append(transitions, t)
	}

	c.log.InfoContext(ctx, "inbound identity bindings changed",
		"chains_changed", len(changed),
		"chains", len(chains),
		"transitions", strings.Join(transitions, " "),
		"snapshot_version", version)
}
