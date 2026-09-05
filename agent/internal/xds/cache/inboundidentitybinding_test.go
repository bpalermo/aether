package cache

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	inboundLineMsg        = "inbound identity binding"
	inboundWarnMsg        = "inbound chain bound to a foreign identity"
	inboundUnservedMsg    = "inbound chain references a secret absent from the snapshot"
	inboundSummaryMsg     = "inbound identity bindings changed"
	inboundMismatchCtr    = "aether.agent.identity.inbound_binding_mismatch"
	inboundEchoIdentity   = "spiffe://aether.internal/ns/aether-test/sa/echo"
	inboundSvc5Identity   = "spiffe://aether.internal/ns/aether-test/sa/svc-5"
	inboundTrustBundleSDS = "spiffe://aether.internal"
)

// serveSecrets installs the named SDS secrets so a chain referencing one counts
// as served (the agent's SPIRE bridge does this via SetSecrets).
func serveSecrets(c *SnapshotCache, names ...string) {
	c.secretMu.Lock()
	if c.secrets == nil {
		c.secrets = make(map[string]*tlsv3.Secret, len(names))
	}
	for _, n := range names {
		c.secrets[n] = &tlsv3.Secret{Name: n}
	}
	c.secretMu.Unlock()
}

// TestInboundIdentityBindingFirstSightThenSilent asserts every inbound chain of
// a pod is named once when first seen — with the server certificate read back
// out of the listener proto, not out of a Go variable — and that repeated
// snapshots then say nothing (issue #638: steady state must be quiet so a
// startup re-bind is greppable).
func TestInboundIdentityBindingFirstSightThenSilent(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	serveSecrets(c, inboundEchoIdentity, inboundTrustBundleSDS)
	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))

	lines := rec.with(inboundLineMsg)
	require.NotEmpty(t, lines, "the pod's inbound chains must be named on first sight")
	for _, l := range lines {
		assert.Equal(t, slog.LevelInfo, l.level)
		assert.Equal(t, "aether-test/echo-1", l.attrs["pod"])
		assert.Equal(t, inboundEchoIdentity, l.attrs["pod_spiffe_id"])
		assert.Equal(t, inboundEchoIdentity, l.attrs["secret"], "the chain must present its own pod's SVID")
		assert.Equal(t, "true", l.attrs["secret_served"])
		assert.Empty(t, l.attrs["previous_secret"], "a first bind has no previous secret")
		assert.Contains(t, l.attrs["chain"], "inbound_echo-1/")
		assert.NotEmpty(t, l.attrs["snapshot_version"])
	}
	assert.Empty(t, rec.with(inboundWarnMsg))
	assert.Zero(t, counterValue(t, reader, inboundMismatchCtr))

	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(inboundLineMsg), "steady state must emit no inbound binding lines")
	assert.Empty(t, rec.with(inboundSummaryMsg))
}

// TestInboundIdentityBindingLogsOnChange asserts a legitimate re-bind — the
// netns is taken over by a pod of a different ServiceAccount, so both the
// listener and the pod change together — logs the transition once per chain and
// is not a mismatch.
func TestInboundIdentityBindingLogsOnChange(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	serveSecrets(c, inboundEchoIdentity, inboundSvc5Identity, inboundTrustBundleSDS)
	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))

	rec.reset()
	require.NoError(t, c.AddPod(ctx, bindingPod("svc-5-1", "svc-5"), bindingTrustDomain))

	lines := rec.with(inboundLineMsg)
	require.NotEmpty(t, lines)
	for _, l := range lines {
		assert.Equal(t, "aether-test/svc-5-1", l.attrs["pod"])
		assert.Equal(t, inboundSvc5Identity, l.attrs["secret"])
	}
	assert.Empty(t, rec.with(inboundWarnMsg), "a consistent re-bind is not a mismatch")
	assert.Zero(t, counterValue(t, reader, inboundMismatchCtr))
}

// TestInboundIdentityBindingForeignIdentityWarns constructs the inverted #638
// failure mode directly: a listener entry whose inbound listener was built for
// a co-located workload while the pod recorded as owning it is a different one
// — i.e. the node would terminate mesh mTLS for echo-1 presenting svc-5's SVID,
// which is precisely what a caller's ssl_fail_verify_san rejects.
func TestInboundIdentityBindingForeignIdentityWarns(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	serveSecrets(c, inboundEchoIdentity, inboundSvc5Identity, inboundTrustBundleSDS)
	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))

	rec.reset()
	foreignPod := bindingPod("svc-5-1", "svc-5")
	foreign, err := proxy.NewInboundListener(foreignPod, bindingTrustDomain, false, false, nil, nil)
	require.NoError(t, err)

	c.listenerMu.Lock()
	entry := c.listeners[bindingNetns]
	entry.inbound = foreign
	c.listeners[bindingNetns] = entry
	c.listenerMu.Unlock()
	require.NoError(t, c.generateSnapshot(ctx))

	warns := rec.with(inboundWarnMsg)
	require.NotEmpty(t, warns)
	for _, w := range warns {
		assert.Equal(t, slog.LevelWarn, w.level)
		assert.Equal(t, "aether-test/echo-1", w.attrs["pod"])
		assert.Equal(t, inboundEchoIdentity, w.attrs["pod_spiffe_id"])
		assert.Equal(t, inboundSvc5Identity, w.attrs["bound_spiffe_id"])
		assert.NotEmpty(t, w.attrs["snapshot_version"])
	}
	assert.Equal(t, int64(len(warns)), counterValue(t, reader, inboundMismatchCtr))

	// A persistent mismatch is not re-counted while nothing changes.
	rec.reset()
	before := counterValue(t, reader, inboundMismatchCtr)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(inboundWarnMsg))
	assert.Equal(t, before, counterValue(t, reader, inboundMismatchCtr))
}

// TestInboundIdentityBindingUnservedSecretWarns covers the other candidate
// mechanism: the chain reaches Envoy before its own SVID is in the snapshot's
// secret set. It is reported but is not a foreign bind, so it is not counted.
func TestInboundIdentityBindingUnservedSecretWarns(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	// No secrets served: the chains exist, their certificate does not.
	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))

	unserved := rec.with(inboundUnservedMsg)
	require.NotEmpty(t, unserved)
	assert.Equal(t, slog.LevelWarn, unserved[0].level)
	assert.Equal(t, "aether-test/echo-1", unserved[0].attrs["pod"])
	assert.Equal(t, inboundEchoIdentity, unserved[0].attrs["secret"])
	assert.Empty(t, rec.with(inboundWarnMsg), "an unserved secret is not a foreign identity")
	assert.Zero(t, counterValue(t, reader, inboundMismatchCtr))

	// The SVID landing re-binds the chains: a change line, then silence.
	rec.reset()
	serveSecrets(c, inboundEchoIdentity, inboundTrustBundleSDS)
	require.NoError(t, c.generateSnapshot(ctx))
	lines := rec.with(inboundLineMsg)
	require.NotEmpty(t, lines)
	assert.Equal(t, "true", lines[0].attrs["secret_served"])
	assert.Empty(t, rec.with(inboundUnservedMsg))
}

// TestInboundIdentityBindingCleartextSilent asserts the SPIRE-off posture emits
// nothing: cleartext inbound chains carry no transport socket, so there is no
// server certificate to bind or to mis-bind.
func TestInboundIdentityBindingCleartextSilent(t *testing.T) {
	c, rec, _ := newBindingTestCache(t)
	ctx := context.Background()

	c.SetSpireEnabled(false)
	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))

	assert.Empty(t, rec.with(inboundLineMsg))
	assert.Empty(t, rec.with(inboundWarnMsg))
	assert.Empty(t, rec.with(inboundUnservedMsg))
}

// TestInboundIdentityBindingSummaryAboveThreshold asserts the rate guard: a
// snapshot binding more than maxBindingChangeLines chains logs one summary
// carrying the distinct pod→identity transitions instead of flooding.
func TestInboundIdentityBindingSummaryAboveThreshold(t *testing.T) {
	c, rec, _ := newBindingTestCache(t)
	ctx := context.Background()

	// Enough pods that their chains exceed the guard in a single snapshot
	// (every pod contributes at least one certificate-bearing chain).
	pods := maxBindingChangeLines + 1
	c.listenerMu.Lock()
	for i := range pods {
		pod := &cniv1.CNIPod{
			Name:             "echo-" + string(rune('a'+i%26)) + "-" + string(rune('a'+i/26)),
			Namespace:        "aether-test",
			ServiceAccount:   "echo",
			NetworkNamespace: "/var/run/netns/cni-bulk-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
		}
		l, err := proxy.NewInboundListener(pod, bindingTrustDomain, false, false, nil, nil)
		require.NoError(t, err)
		c.listeners[pod.GetNetworkNamespace()] = listenerEntry{inbound: l, cniPod: pod}
	}
	c.listenerMu.Unlock()
	c.localMu.Lock()
	c.trustDomain = bindingTrustDomain
	c.localMu.Unlock()

	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	assert.Empty(t, rec.with(inboundLineMsg), "the per-chain lines are suppressed above the guard")
	summaries := rec.with(inboundSummaryMsg)
	require.Len(t, summaries, 1)
	assert.Contains(t, summaries[0].attrs["transitions"], inboundEchoIdentity)
	assert.NotEmpty(t, summaries[0].attrs["chains_changed"])
}
