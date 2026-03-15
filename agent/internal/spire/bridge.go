package spire

import (
	"context"
	"fmt"
	"sync"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/go-logr/logr"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	typesv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
)

// SecretStore is the interface for pushing secrets into the xDS snapshot cache.
type SecretStore interface {
	SetSecrets(ctx context.Context, secrets []*tlsv3.Secret) error
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

	mu      sync.RWMutex
	secrets map[string]*tlsv3.Secret // keyed by secret name (SPIFFE ID or trust domain)

	// subscriptions tracks active SVID subscriptions keyed by SPIFFE ID.
	// Each entry holds a cancel function to stop the subscription.
	subsMu        sync.Mutex
	subscriptions map[string]context.CancelFunc

	// ctx is the bridge's root context, set during Start.
	ctx context.Context
}

// NewBridge creates a new SPIRE bridge.
func NewBridge(socketPath string, store SecretStore, log logr.Logger) *Bridge {
	return &Bridge{
		socketPath:    socketPath,
		store:         store,
		log:           log.WithName("spire-bridge"),
		secrets:       make(map[string]*tlsv3.Secret),
		subscriptions: make(map[string]context.CancelFunc),
	}
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
		_ = client.Close()
	}()

	b.log.Info("connected to SPIRE agent", "socket", b.socketPath)

	bundleCh, err := client.SubscribeBundles(ctx)
	if err != nil {
		return fmt.Errorf("subscribing to bundles: %w", err)
	}

	b.log.Info("subscribed to X.509 bundles")

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

// SubscribePod starts an SVID subscription for the given pod. The selectors
// identify the workload to the SPIRE agent. The SPIFFE ID is used as the
// secret name for Envoy. It is a no-op if the bridge has not been started yet.
func (b *Bridge) SubscribePod(ctx context.Context, spiffeID string, selectors []*typesv1.Selector) error {
	if b.client == nil {
		b.log.V(1).Info("bridge not started, skipping SVID subscription", "spiffeID", spiffeID)
		return nil
	}

	b.subsMu.Lock()
	if _, exists := b.subscriptions[spiffeID]; exists {
		b.subsMu.Unlock()
		return nil // already subscribed
	}

	subCtx, cancel := context.WithCancel(b.ctx)
	b.subscriptions[spiffeID] = cancel
	b.subsMu.Unlock()

	svidCh, err := b.client.SubscribeSVIDs(subCtx, selectors)
	if err != nil {
		cancel()
		b.subsMu.Lock()
		delete(b.subscriptions, spiffeID)
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
				if handleErr := b.handleSVIDUpdate(ctx, resp); handleErr != nil {
					b.log.Error(handleErr, "handling SVID update", "spiffeID", spiffeID)
				}
			}
		}
	}()

	return nil
}

// UnsubscribePod stops the SVID subscription for the given SPIFFE ID and
// removes its secret from the cache. It is a no-op if the bridge has not
// been started yet.
func (b *Bridge) UnsubscribePod(ctx context.Context, spiffeID string) error {
	if b.client == nil {
		return nil
	}

	b.subsMu.Lock()
	cancel, exists := b.subscriptions[spiffeID]
	if exists {
		cancel()
		delete(b.subscriptions, spiffeID)
	}
	b.subsMu.Unlock()

	b.mu.Lock()
	delete(b.secrets, spiffeID)
	b.mu.Unlock()

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
