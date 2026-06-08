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
	"sync"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/go-logr/logr"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	apitypes "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
)

// nodeSVIDRefreshInterval is how often the bridge re-reads its node SVID from
// the Workload API source to pick up rotations and push the refreshed secret.
const nodeSVIDRefreshInterval = 30 * time.Second

// SecretStore is the interface for pushing secrets into the xDS snapshot cache.
type SecretStore interface {
	SetSecrets(ctx context.Context, secrets []*tlsv3.Secret) error
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
	log        logr.Logger

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
}

// podSubscription is an active delegated-identity subscription for one pod.
type podSubscription struct {
	cancel   context.CancelFunc
	spiffeID string
}

// NewBridge creates a new SPIRE bridge. nodeSource is the agent's own Workload
// API SVID source used to serve the node identity; it may be nil to disable
// node-SVID serving.
func NewBridge(socketPath string, store SecretStore, nodeSource X509SVIDSource, log logr.Logger) *Bridge {
	return &Bridge{
		socketPath:    socketPath,
		store:         store,
		nodeSource:    nodeSource,
		log:           log.WithName("spire-bridge"),
		secrets:       make(map[string]*tlsv3.Secret),
		subscriptions: make(map[string]podSubscription),
	}
}

// NodeSpiffeID returns the SPIFFE ID of the agent's node identity once the node
// SVID has been served, or "" if node-SVID serving is disabled or not yet ready.
// It is the SDS secret name proxies reference for node-to-node tunnel mTLS.
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
			b.log.V(1).Info("failed to close SPIRE client", "error", closeErr)
		}
	}()

	b.log.Info("connected to SPIRE agent", "socket", b.socketPath)

	bundleCh, err := client.SubscribeBundles(ctx)
	if err != nil {
		return fmt.Errorf("subscribing to bundles: %w", err)
	}

	b.log.Info("subscribed to X.509 bundles")

	// Serve the agent's own node SVID (for node-to-node tunnel mTLS and the
	// node-health listener) and keep it refreshed on rotation.
	if b.nodeSource != nil {
		if err := b.refreshNodeSVID(ctx); err != nil {
			b.log.Error(err, "serving initial node SVID")
		}
		go b.runNodeSVIDRefresh(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			b.log.Info("shutting down SPIRE bridge")
			return nil
		case resp, ok := <-bundleCh:
			if !ok {
				return fmt.Errorf("bundle subscription stream closed unexpectedly")
			}
			if err := b.handleBundleUpdate(ctx, resp); err != nil {
				b.log.Error(err, "handling bundle update")
			}
		}
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
	if b.client == nil {
		b.log.V(1).Info("bridge not started, skipping SVID subscription", "spiffeID", spiffeID)
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

	go func() {
		for {
			select {
			case <-subCtx.Done():
				return
			case resp, ok := <-svidCh:
				if !ok {
					b.log.Info("SVID subscription stream closed", "spiffeID", spiffeID)
					return
				}
				// Use subCtx (tied to the bridge/subscription lifetime), not the
				// caller's request context: SubscribePod is called synchronously
				// from CmdAdd, whose context is cancelled as soon as it returns —
				// pushing the SVID into the snapshot must outlive that request.
				if handleErr := b.handleSVIDUpdate(subCtx, resp); handleErr != nil {
					b.log.Error(handleErr, "handling SVID update", "spiffeID", spiffeID)
				}
			}
		}
	}()

	return nil
}

// UnsubscribePod stops the SVID subscription for the pod in the given network
// namespace. The pod's SVID secret is removed only when no other subscribed pod
// shares the same SPIFFE ID (service account), so a rolling restart that briefly
// runs two same-identity pods never drops the live SVID. No-op if the bridge has
// not been started or the netns is not subscribed.
func (b *Bridge) UnsubscribePod(ctx context.Context, netns string) error {
	if b.client == nil {
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

	b.log.V(1).Info("processed bundle update", "trustDomains", len(resp.GetCaCertificates()))

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

	b.log.V(1).Info("processed SVID update", "svids", len(resp.GetX509Svids()))

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
				b.log.V(1).Info("refreshing node SVID", "error", err)
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
	b.nodeSpiffeID = secret.GetName()
	b.mu.Unlock()

	b.log.V(1).Info("served node SVID", "spiffeID", secret.GetName())
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
