// Package spire provides integration with SPIRE for X.509 SVID management.
//
// The SPIRE bridge connects the SPIRE Delegated Identity API to the Aether agent's
// xDS snapshot cache. It subscribes to X.509 SVIDs (Secure Workload Identity Documents)
// and trust bundles from a SPIRE agent and converts them to Envoy Secret resources.
// These secrets are pushed to the xDS cache and delivered to Envoy proxies for mTLS.
//
// The bridge implements controller-runtime's Runnable interface for lifecycle management
// within the agent's Manager. It uses goroutines to handle asynchronous SVID and bundle
// subscription streams from SPIRE.
//
// SPIRE integration is optional and can be disabled via the spire-enabled flag.
// If disabled, the agent skips the SPIRE bridge but still functions normally.
package spire

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	commonlog "github.com/bpalermo/aether/common/log"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	apitypes "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"go.opentelemetry.io/otel"
)

// nodeSVIDRefreshInterval is how often the bridge re-reads its node SVID from
// the Workload API source to pick up rotations and push the refreshed secret.
const nodeSVIDRefreshInterval = 30 * time.Second

// Backoff policy for re-establishing delegated-identity subscription streams
// after a disconnect (e.g. a SPIRE agent restart). Matches the registrar
// watch-stream policy. The backoff resets only once a response is received on
// the new stream, so a stream that connects and immediately closes keeps
// backing off rather than hot-looping.
const (
	initialStreamBackoff = 1 * time.Second
	maxStreamBackoff     = 30 * time.Second
	streamJitterFraction = 0.2
)

// SecretStore is the interface for pushing secrets into the xDS snapshot cache.
type SecretStore interface {
	SetSecrets(ctx context.Context, secrets []*tlsv3.Secret) error
}

// NodeIdentitySink receives the agent's node SPIFFE ID when the node SVID is
// served, so resources that reference it (the outbound clusters' no-match upstream
// client cert) can be generated. The xDS snapshot cache satisfies it; the bridge
// calls it only when its store also implements this interface.
type NodeIdentitySink interface {
	SetNodeIdentity(ctx context.Context, nodeSpiffeID string) error
}

// X509SVIDSource provides the agent's own node SVID. It is satisfied by the
// go-spiffe Workload API X509Source the agent already uses for registrar mTLS.
type X509SVIDSource interface {
	GetX509SVID() (*x509svid.SVID, error)
}

// Bridge connects the SPIRE Delegated Identity API to the xDS snapshot cache.
// It subscribes to X.509 SVIDs and trust bundles from SPIRE and converts them
// to Envoy Secret resources. Bridge implements controller-runtime's Runnable
// interface for lifecycle management.
type Bridge struct {
	socketPath string
	client     *Client
	store      SecretStore
	log        *slog.Logger
	metrics    *bridgeMetrics

	// Stream re-subscribe backoff bounds; set to the package constants in
	// NewBridge, overridable in tests.
	backoffInitial time.Duration
	backoffMax     time.Duration

	// nodeSource provides the agent's own SVID (the node identity). When set,
	// the bridge serves it as an SDS secret named by its SPIFFE ID and refreshes
	// it on rotation. nodeSpiffeID caches that name for config references.
	nodeSource   X509SVIDSource
	nodeSpiffeID string

	mu      sync.RWMutex
	secrets map[string]*tlsv3.Secret // keyed by secret name (SPIFFE ID or trust domain)

	// subscriptions tracks active SVID subscriptions keyed by the pod's network
	// namespace (unique per pod), not its SPIFFE ID: pods sharing a service
	// account share a SPIFFE ID, and keying by it would let a terminating pod's
	// unsubscribe cancel a newer same-identity pod's subscription during a rolling
	// restart. The SVID secret (named by SPIFFE ID) is kept until the last
	// subscription referencing it goes away.
	subsMu        sync.Mutex
	subscriptions map[string]podSubscription // keyed by network namespace

	// ctx is the bridge's root context, set during Start.
	ctx context.Context

	// started is closed once Start has connected to the SPIRE agent and the
	// bridge can accept subscriptions (SubscribePod no-ops before then).
	started chan struct{}
}

// podSubscription is an active delegated-identity subscription for one pod.
type podSubscription struct {
	cancel   context.CancelFunc
	spiffeID string
}

// NewBridge creates a new SPIRE bridge. nodeSource is the agent's own Workload
// API SVID source used to serve the node identity; it may be nil to disable
// node-SVID serving.
func NewBridge(socketPath string, store SecretStore, nodeSource X509SVIDSource, log *slog.Logger) *Bridge {
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the bridge.
	metrics, err := newBridgeMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create SPIRE bridge metrics; continuing without instrumentation", "error", err)
	}

	return &Bridge{
		socketPath:     socketPath,
		store:          store,
		nodeSource:     nodeSource,
		log:            commonlog.Named(log, "spire-bridge"),
		metrics:        metrics,
		backoffInitial: initialStreamBackoff,
		backoffMax:     maxStreamBackoff,
		secrets:        make(map[string]*tlsv3.Secret),
		subscriptions:  make(map[string]podSubscription),
		started:        make(chan struct{}),
	}
}

// Started returns a channel that is closed once the bridge has connected to the
// SPIRE agent and can accept subscriptions. Callers re-subscribing stored pods
// after an agent restart wait on it (selecting on their context as well, since
// the channel never closes if Start fails before connecting).
func (b *Bridge) Started() <-chan struct{} {
	return b.started
}

// NodeSpiffeID returns the SPIFFE ID of the agent's node identity once the node
// SVID has been served, or "" if node-SVID serving is disabled or not yet ready.
// It is the SDS secret name proxies reference for node-originated upstream mTLS
// (the outbound clusters' no-match client cert) and the node-health listener.
func (b *Bridge) NodeSpiffeID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.nodeSpiffeID
}

// Start connects to the SPIRE agent and begins subscribing to trust bundles.
// It blocks until the context is canceled. Implements controller-runtime Runnable.
func (b *Bridge) Start(ctx context.Context) error {
	b.ctx = ctx

	client, err := NewClient(ctx, b.socketPath, b.log)
	if err != nil {
		return fmt.Errorf("creating SPIRE client: %w", err)
	}
	b.client = client
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			b.log.DebugContext(ctx, "failed to close SPIRE client", "error", closeErr)
		}
	}()

	b.log.InfoContext(ctx, "connected to SPIRE agent", "socket", b.socketPath)
	close(b.started)

	// Serve the agent's own node SVID (for node-originated upstream mTLS and the
	// node-health listener) and keep it refreshed on rotation.
	if b.nodeSource != nil {
		if err := b.refreshNodeSVID(ctx); err != nil {
			b.log.ErrorContext(ctx, "serving initial node SVID", "error", err)
		}
		go b.runNodeSVIDRefresh(ctx)
	}

	return b.runBundleSubscriptionLoop(ctx, client)
}

// runBundleSubscriptionLoop maintains the bundle subscription for the bridge's
// lifetime, re-subscribing with backoff when the stream ends (e.g. the SPIRE
// agent restarts) instead of failing the runnable — which would take down the
// whole agent for a transient disconnect. SPIRE sends the full bundle set as
// the first response on every new stream, so a plain re-subscribe fully
// resynchronizes; the cached bundle secrets keep serving while disconnected.
func (b *Bridge) runBundleSubscriptionLoop(ctx context.Context, client *Client) error {
	backoff := b.backoffInitial
	first := true
	for {
		bundleCh, subErr := client.SubscribeBundles(ctx)
		if subErr != nil {
			ok, newBackoff := b.handleBundleSubscribeError(ctx, subErr, backoff)
			if !ok {
				return nil
			}
			backoff = newBackoff
			first = false
			continue
		}
		b.logBundleSubscribed(ctx, first)
		first = false

		done, newBackoff := b.drainBundleStream(ctx, bundleCh, backoff)
		if done {
			return nil
		}
		backoff = newBackoff

		b.metrics.streamFailed(ctx, streamBundle)
		wait := jitteredBackoff(backoff)
		b.log.InfoContext(ctx, "bundle subscription stream closed; re-subscribing", "backoff", wait)
		if !sleepCtx(ctx, wait) {
			b.log.InfoContext(ctx, "shutting down SPIRE bridge")
			return nil
		}
		backoff = min(backoff*2, b.backoffMax)
	}
}

// handleBundleSubscribeError handles a subscription error in runBundleSubscriptionLoop.
// Returns (ok=false) if the context is cancelled and the loop should exit,
// otherwise waits with backoff and returns (true, updatedBackoff).
func (b *Bridge) handleBundleSubscribeError(ctx context.Context, subErr error, backoff time.Duration) (ok bool, newBackoff time.Duration) {
	if ctx.Err() != nil {
		b.log.InfoContext(ctx, "shutting down SPIRE bridge")
		return false, backoff
	}
	b.metrics.streamFailed(ctx, streamBundle)
	wait := jitteredBackoff(backoff)
	b.log.ErrorContext(ctx, "subscribing to X.509 bundles failed; retrying", "error", subErr, "backoff", wait)
	if !sleepCtx(ctx, wait) {
		b.log.InfoContext(ctx, "shutting down SPIRE bridge")
		return false, backoff
	}
	return true, min(backoff*2, b.backoffMax)
}

// logBundleSubscribed emits the appropriate log line for a successful bundle subscription.
func (b *Bridge) logBundleSubscribed(ctx context.Context, first bool) {
	if first {
		b.log.InfoContext(ctx, "subscribed to X.509 bundles")
	} else {
		b.metrics.streamReconnected(ctx, streamBundle)
		b.log.InfoContext(ctx, "re-subscribed to X.509 bundles")
	}
}

// drainBundleStream reads from bundleCh until the channel is closed or the
// context is cancelled. Returns (done=true) when the context is cancelled
// (caller should return nil), and the updated backoff (reset to initial on
// each successful receive).
func (b *Bridge) drainBundleStream(ctx context.Context, bundleCh <-chan *delegatedidentityv1.SubscribeToX509BundlesResponse, backoff time.Duration) (done bool, newBackoff time.Duration) {
	for {
		select {
		case <-ctx.Done():
			b.log.InfoContext(ctx, "shutting down SPIRE bridge")
			return true, backoff
		case resp, ok := <-bundleCh:
			if !ok {
				return false, backoff
			}
			// Receiving proves the stream is healthy; reset the backoff.
			backoff = b.backoffInitial
			if err := b.handleBundleUpdate(ctx, resp); err != nil {
				b.log.ErrorContext(ctx, "handling bundle update", "error", err)
			}
		}
	}
}

// jitteredBackoff returns d plus up to streamJitterFraction of random jitter,
// de-synchronizing re-subscribe attempts across streams and agents.
func jitteredBackoff(d time.Duration) time.Duration {
	return d + time.Duration(float64(d)*streamJitterFraction*rand.Float64())
}

// sleepCtx waits for d or until ctx is done; it reports whether the full wait
// elapsed (false means ctx was cancelled).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// PodSelectors builds the SPIRE k8s workload selectors that identify a pod by
// its namespace, service account, name and UID. SPIRE issues the SVID of any
// registration entry whose selectors are a subset of these; the
// spire-controller-manager binds entries by k8s:pod-uid, which is unique per pod.
func PodSelectors(namespace, serviceAccount, podName, uid string) []*apitypes.Selector {
	sel := make([]*apitypes.Selector, 0, 4)
	if namespace != "" {
		sel = append(sel, &apitypes.Selector{Type: "k8s", Value: "ns:" + namespace})
	}
	if serviceAccount != "" {
		sel = append(sel, &apitypes.Selector{Type: "k8s", Value: "sa:" + serviceAccount})
	}
	if podName != "" {
		sel = append(sel, &apitypes.Selector{Type: "k8s", Value: "pod-name:" + podName})
	}
	if uid != "" {
		sel = append(sel, &apitypes.Selector{Type: "k8s", Value: "pod-uid:" + uid})
	}
	return sel
}

// SubscribePod starts an SVID subscription for the pod in the given network
// namespace using its Kubernetes workload selectors (namespace, service account,
// pod name and UID). The SPIRE agent returns the SVIDs of every registration
// entry whose selectors are satisfied — no process attestation, so no container
// PID is required. spiffeID is used as the secret name for Envoy. It is a no-op
// if the bridge has not been started yet or the netns is already subscribed. The
// subscription is bound to the bridge's lifetime, not any request context.
func (b *Bridge) SubscribePod(netns, spiffeID string, selectors []*apitypes.Selector) error {
	// Gate on the started channel rather than a bare b.client nil-check: the
	// close(b.started) in Start happens-after the client/ctx assignments, so this
	// select also synchronizes their reads (a plain nil-check is a data race).
	select {
	case <-b.started:
	default:
		b.log.Debug("bridge not started, skipping SVID subscription", "spiffeID", spiffeID)
		return nil
	}

	b.subsMu.Lock()
	if _, exists := b.subscriptions[netns]; exists {
		b.subsMu.Unlock()
		return nil // already subscribed for this pod
	}

	subCtx, cancel := context.WithCancel(b.ctx)
	b.subscriptions[netns] = podSubscription{cancel: cancel, spiffeID: spiffeID}
	b.subsMu.Unlock()

	svidCh, err := b.client.SubscribeSVIDsBySelectors(subCtx, selectors)
	if err != nil {
		cancel()
		b.subsMu.Lock()
		delete(b.subscriptions, netns)
		b.subsMu.Unlock()
		return fmt.Errorf("subscribing to SVIDs for %s: %w", spiffeID, err)
	}

	b.log.Info("subscribed to SVIDs", "spiffeID", spiffeID)

	go b.runSVIDSubscriptionLoop(subCtx, svidCh, spiffeID, selectors)

	return nil
}

// runSVIDSubscriptionLoop is the goroutine that drives an SVID subscription for
// one pod. It reads from the initial channel and re-subscribes with backoff
// whenever the stream closes (e.g. the SPIRE agent restarts). It exits when
// subCtx is cancelled.
func (b *Bridge) runSVIDSubscriptionLoop(subCtx context.Context, svidCh <-chan *delegatedidentityv1.SubscribeToX509SVIDsResponse, spiffeID string, selectors []*apitypes.Selector) {
	ch := svidCh
	backoff := b.backoffInitial
	for {
		backoff = b.drainSVIDStream(subCtx, ch, spiffeID, backoff)
		if subCtx.Err() != nil {
			return
		}

		// The stream ended while the pod is still subscribed (e.g. the SPIRE
		// agent restarted): re-subscribe with backoff so rotations keep
		// flowing — exiting here would silently freeze this pod's SVID until
		// it expired. The cached SVID keeps serving meanwhile, and the first
		// response on the new stream is the current SVID set, so a plain
		// re-subscribe fully resynchronizes.
		b.metrics.streamFailed(subCtx, streamSVID)
		newCh, ok := b.resubscribeSVIDs(subCtx, selectors, spiffeID, &backoff)
		if !ok {
			return
		}
		ch = newCh
	}
}

// drainSVIDStream reads responses from ch until it is closed or subCtx is
// cancelled. Resets backoff to initial on each successful receive. Returns the
// updated backoff.
func (b *Bridge) drainSVIDStream(subCtx context.Context, ch <-chan *delegatedidentityv1.SubscribeToX509SVIDsResponse, spiffeID string, backoff time.Duration) time.Duration {
	for {
		select {
		case <-subCtx.Done():
			return backoff
		case resp, ok := <-ch:
			if !ok {
				return backoff
			}
			// Receiving proves the stream is healthy; reset the backoff.
			backoff = b.backoffInitial
			// Use subCtx (tied to the bridge/subscription lifetime), not the
			// caller's request context: SubscribePod is called synchronously
			// from CmdAdd, whose context is cancelled as soon as it returns —
			// pushing the SVID into the snapshot must outlive that request.
			if handleErr := b.handleSVIDUpdate(subCtx, resp); handleErr != nil {
				b.log.Error("handling SVID update", "error", handleErr, "spiffeID", spiffeID)
			}
		}
	}
}

// resubscribeSVIDs retries subscribing to SVIDs with backoff until it succeeds
// or subCtx is cancelled. Returns (channel, true) on success, (nil, false) when
// the context was cancelled.
func (b *Bridge) resubscribeSVIDs(subCtx context.Context, selectors []*apitypes.Selector, spiffeID string, backoff *time.Duration) (<-chan *delegatedidentityv1.SubscribeToX509SVIDsResponse, bool) {
	for {
		wait := jitteredBackoff(*backoff)
		b.log.Info("SVID subscription stream closed; re-subscribing", "spiffeID", spiffeID, "backoff", wait)
		if !sleepCtx(subCtx, wait) {
			return nil, false
		}
		*backoff = min(*backoff*2, b.backoffMax)
		newCh, subErr := b.client.SubscribeSVIDsBySelectors(subCtx, selectors)
		if subErr != nil {
			b.metrics.streamFailed(subCtx, streamSVID)
			b.log.Error("re-subscribing to SVIDs failed; retrying", "error", subErr, "spiffeID", spiffeID)
			continue
		}
		b.metrics.streamReconnected(subCtx, streamSVID)
		b.log.Info("re-subscribed to SVIDs", "spiffeID", spiffeID)
		return newCh, true
	}
}

// UnsubscribePod stops the SVID subscription for the pod in the given network
// namespace. The pod's SVID secret is removed only when no other subscribed pod
// shares the same SPIFFE ID (service account), so a rolling restart that briefly
// runs two same-identity pods never drops the live SVID. No-op if the bridge has
// not been started or the netns is not subscribed.
func (b *Bridge) UnsubscribePod(ctx context.Context, netns string) error {
	// See SubscribePod: the started gate synchronizes client/ctx reads.
	select {
	case <-b.started:
	default:
		return nil
	}

	b.subsMu.Lock()
	sub, exists := b.subscriptions[netns]
	if !exists {
		b.subsMu.Unlock()
		return nil
	}
	sub.cancel()
	delete(b.subscriptions, netns)

	// Keep the secret while another pod still references the same SPIFFE ID.
	stillReferenced := false
	for _, other := range b.subscriptions {
		if other.spiffeID == sub.spiffeID {
			stillReferenced = true
			break
		}
	}
	b.subsMu.Unlock()

	if !stillReferenced {
		b.mu.Lock()
		delete(b.secrets, sub.spiffeID)
		b.mu.Unlock()
	}

	return b.pushSecrets(ctx)
}

// handleBundleUpdate processes a bundle update from SPIRE and converts each
// trust domain's CA certificates into an Envoy validation context Secret.
func (b *Bridge) handleBundleUpdate(ctx context.Context, resp *delegatedidentityv1.SubscribeToX509BundlesResponse) error {
	b.mu.Lock()
	// Remove old bundle secrets (trust domain keys)
	for name, secret := range b.secrets {
		if _, isValidation := secret.Type.(*tlsv3.Secret_ValidationContext); isValidation {
			delete(b.secrets, name)
		}
	}

	for trustDomain, derCerts := range resp.GetCaCertificates() {
		secret, err := BundleToValidationContextSecret(trustDomain, derCerts)
		if err != nil {
			b.mu.Unlock()
			return fmt.Errorf("converting bundle for %s: %w", trustDomain, err)
		}
		b.secrets[trustDomain] = secret
	}
	b.mu.Unlock()

	b.log.DebugContext(ctx, "processed bundle update", "trustDomains", len(resp.GetCaCertificates()))

	return b.pushSecrets(ctx)
}

// handleSVIDUpdate processes an SVID update from SPIRE and converts each SVID
// into an Envoy TLS certificate Secret.
func (b *Bridge) handleSVIDUpdate(ctx context.Context, resp *delegatedidentityv1.SubscribeToX509SVIDsResponse) error {
	b.mu.Lock()
	for _, svidWithKey := range resp.GetX509Svids() {
		secret, err := SVIDToTLSCertificateSecret(svidWithKey)
		if err != nil {
			b.mu.Unlock()
			return fmt.Errorf("converting SVID: %w", err)
		}
		b.secrets[secret.Name] = secret
	}
	b.mu.Unlock()

	b.log.DebugContext(ctx, "processed SVID update", "svids", len(resp.GetX509Svids()))

	return b.pushSecrets(ctx)
}

// runNodeSVIDRefresh periodically re-reads and re-serves the node SVID so
// rotations are pushed to Envoy. It returns when the context is cancelled.
func (b *Bridge) runNodeSVIDRefresh(ctx context.Context) {
	ticker := time.NewTicker(nodeSVIDRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.refreshNodeSVID(ctx); err != nil {
				b.log.DebugContext(ctx, "refreshing node SVID", "error", err)
			}
		}
	}
}

// refreshNodeSVID reads the current node SVID from the Workload API source and,
// if it changed, updates the cached secret and pushes it. It is a no-op when the
// node source is unset.
func (b *Bridge) refreshNodeSVID(ctx context.Context) error {
	if b.nodeSource == nil {
		return nil
	}

	svid, err := b.nodeSource.GetX509SVID()
	if err != nil {
		return fmt.Errorf("fetching node SVID: %w", err)
	}

	secret, err := X509SVIDToTLSCertificateSecret(svid)
	if err != nil {
		return fmt.Errorf("converting node SVID: %w", err)
	}

	b.mu.Lock()
	if existing, ok := b.secrets[secret.GetName()]; ok && secretsEqual(existing, secret) {
		b.mu.Unlock()
		return nil // unchanged; avoid a no-op snapshot bump
	}
	b.secrets[secret.GetName()] = secret
	firstServe := b.nodeSpiffeID == ""
	b.nodeSpiffeID = secret.GetName()
	b.mu.Unlock()

	// Inform the cache of the node identity so outbound clusters (which reference
	// it as the no-match upstream client cert) can be generated. Only needed once:
	// the SPIFFE ID is stable across rotations.
	if firstServe {
		if sink, ok := b.store.(NodeIdentitySink); ok {
			if err := sink.SetNodeIdentity(ctx, secret.GetName()); err != nil {
				b.log.ErrorContext(ctx, "setting node identity on cache", "error", err, "spiffeID", secret.GetName())
			}
		}
	}

	b.log.DebugContext(ctx, "served node SVID", "spiffeID", secret.GetName())
	return b.pushSecrets(ctx)
}

// secretsEqual reports whether two TLS-certificate secrets carry the same cert
// chain and key, used to skip pushing an unchanged node SVID.
func secretsEqual(a, b *tlsv3.Secret) bool {
	ac, bc := a.GetTlsCertificate(), b.GetTlsCertificate()
	if ac == nil || bc == nil {
		return false
	}
	return bytes.Equal(ac.GetCertificateChain().GetInlineBytes(), bc.GetCertificateChain().GetInlineBytes()) &&
		bytes.Equal(ac.GetPrivateKey().GetInlineBytes(), bc.GetPrivateKey().GetInlineBytes())
}

// pushSecrets collects all current secrets and pushes them to the snapshot cache.
func (b *Bridge) pushSecrets(ctx context.Context) error {
	b.mu.RLock()
	secrets := make([]*tlsv3.Secret, 0, len(b.secrets))
	for _, s := range b.secrets {
		secrets = append(secrets, s)
	}
	b.mu.RUnlock()

	return b.store.SetSecrets(ctx, secrets)
}
