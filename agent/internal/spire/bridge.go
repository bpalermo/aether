// Package spire provides integration with SPIRE for X.509 SVID management.
//
// The SPIRE bridge connects the SPIFFE Broker API to the Aether agent's xDS
// snapshot cache. For every pod on this node it opens a Broker subscription
// carrying a KubernetesObjectReference; the SPIRE agent resolves and attests the
// referenced pod itself and streams its X.509-SVIDs, which the bridge converts
// to Envoy Secret resources. Validation contexts come from the agent's OWN
// Workload API trust bundle plus the union of the federated bundles those pod
// streams carry. The secrets are pushed to the xDS cache and delivered to Envoy
// proxies for mTLS.
//
// The bridge implements controller-runtime's Runnable interface for lifecycle
// management within the agent's Manager. It uses goroutines to handle the
// asynchronous per-pod subscription streams.
//
// SPIRE integration is optional and can be disabled via the spire-enabled flag.
// If disabled, the agent skips the SPIRE bridge but still functions normally.
package spire

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"sync"
	"time"

	commonlog "aethermesh.dev/common/log"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// identityRefreshInterval is how often the bridge re-reads its own SVID and
// trust bundle from the Workload API source to pick up rotations and push the
// refreshed secrets. It is a backstop: the source's update signal normally wakes
// the refresher the instant either changes.
const identityRefreshInterval = 30 * time.Second

// referenceUnresolvedWarnAfter is how long a pod reference may keep coming back
// unresolved before the per-attempt log line escalates from INFO to WARN.
//
// A brief NotFound is EXPECTED and benign: the Broker API resolves the reference
// at request time, so a CNI ADD can legitimately beat the pod into the kubelet's
// list — a race the selector-based delegated path never had, because it never
// resolved anything. What is not benign is a reference that never resolves, and
// the only thing that distinguishes the two is how long it has been going on.
const referenceUnresolvedWarnAfter = 30 * time.Second

// brokerUnreachableErrorAfter is how long subscribes may keep failing with
// Unavailable before the per-attempt line escalates from WARN to ERROR.
//
// A brief Unavailable is designed, twice over: a restarted agent runs its
// stored-pod resubscribe before its own SVID has landed, so the Broker's mutual
// TLS has no client certificate for a second or so (#740); and a SPIRE agent
// restart takes the socket away for ~35s. Both logged one ERROR per managed pod
// per attempt and healed at zero cost (#766). Two minutes matches the readiness
// dwell: past it the node is NotReady and an operator is already being told.
const brokerUnreachableErrorAfter = 2 * time.Minute

// Backoff policy for re-establishing Broker subscription streams after a failed
// subscribe or a disconnect (e.g. a SPIRE agent restart). Matches the registrar
// watch-stream policy. The backoff resets only once a subscribe succeeds, so a
// stream that connects and immediately closes keeps backing off rather than
// hot-looping.
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

// UpdatedSource is the optional extension of IdentitySource that announces when
// the SVID or the trust bundle changed. When the source implements it, the bridge
// serves the node identity the instant it arrives instead of on the next 30s tick
// — which is what keeps a late SVID (SPIRE still coming up at boot) from costing
// the node up to half a minute of upstream mTLS after identity is finally
// available. Both workloadapi.X509Source and spire.WaitingSource satisfy it.
type UpdatedSource interface {
	Updated() <-chan struct{}
}

// newClientFunc builds the Broker client. It is a Bridge field so tests can
// substitute a client without a socket.
type newClientFunc func(socketPath string, source IdentitySource, log *slog.Logger) (BrokerClient, error)

// Bridge connects the SPIFFE Broker API to the xDS snapshot cache. It subscribes
// to per-pod X.509 SVIDs, derives trust bundles from the agent's own identity and
// the pods' federated bundles, and converts both to Envoy Secret resources.
// Bridge implements controller-runtime's Runnable interface.
type Bridge struct {
	socketPath string
	client     BrokerClient
	newClient  newClientFunc
	store      SecretStore
	log        *slog.Logger
	metrics    *bridgeMetrics

	// Stream re-subscribe backoff bounds; set to the package constants in
	// NewBridge, overridable in tests.
	backoffInitial time.Duration
	backoffMax     time.Duration

	// source is the agent's own Workload API identity: the node SVID it serves,
	// the client certificate for the mutually-authenticated Broker Endpoint, and
	// the trust bundle every validation context starts from. nodeSpiffeID caches
	// the served node identity's name for config references.
	source       IdentitySource
	nodeSpiffeID string

	// mu guards secrets, gen and the two bundle INPUTS below. Every mutation of
	// secrets bumps gen, so a generation identifies exactly one state of the
	// secret set.
	mu      sync.RWMutex
	secrets map[string]*tlsv3.Secret // keyed by secret name (SPIFFE ID or trust domain)
	gen     uint64

	// ownBundles is the agent's own Workload API trust bundle (at most one entry,
	// keyed by the canonical trust-domain SPIFFE URI) and podBundles is the
	// per-pod federated bundle map keyed by network namespace. The served
	// validation contexts are DERIVED from their union, so a pod going away drops
	// exactly its own contribution — see rebuildValidationContextsLocked.
	//
	// The Broker API cannot serve a node-wide bundle stream: SubscribeToX509Bundles
	// also takes a workload reference, and a node with zero managed pods has none.
	// Hence the agent's own identity, which exists as soon as SPIRE attests the
	// agent itself, is the authoritative source for its trust domain.
	ownBundles map[string][]byte
	podBundles map[string]map[string][]byte

	// podSVIDs remembers the certificate chain last served for each subscribed
	// pod (by network namespace), which is what tells a rotation from a first
	// delivery and from a redelivery after a re-subscribe. Guarded by mu.
	podSVIDs map[string][]byte

	// pushMu serialises pushSecrets. It is held across BOTH the snapshot of
	// secrets and the store publish, which is what makes publication ordered:
	// several independent goroutines push (every per-pod SVID stream, the
	// identity refresher and UnsubscribePod), and before this lock existed they
	// built a slice, released mu, and raced into SetSecrets — a whole-map
	// replace, so the loser overwrote the winner and a just-arrived secret
	// vanished from SDS until the next push (issue #772, S10).
	//
	// publishedGen is the generation the store currently holds; it only ever
	// moves forward, so a push that observed an older generation is rejected
	// rather than published.
	pushMu       sync.Mutex
	publishedGen uint64

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

	// started is closed once Start has built the Broker client and the bridge can
	// accept subscriptions (SubscribePod no-ops before then).
	started chan struct{}
}

// podSubscription is an active Broker subscription for one pod.
type podSubscription struct {
	cancel   context.CancelFunc
	spiffeID string
	ref      PodRef
}

// NewBridge creates a new SPIRE bridge. source is the agent's own Workload API
// identity, used for the Broker Endpoint's mutual TLS, for the node identity
// secret, and for the trust bundle. It may be nil only in tests that never Start.
func NewBridge(socketPath string, store SecretStore, source IdentitySource, log *slog.Logger) *Bridge {
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the bridge.
	metrics, err := newBridgeMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create SPIRE bridge metrics; continuing without instrumentation", "error", err)
	}

	return &Bridge{
		socketPath: socketPath,
		newClient: func(socketPath string, source IdentitySource, log *slog.Logger) (BrokerClient, error) {
			return newBrokerClient(socketPath, source, log)
		},
		store:          store,
		source:         source,
		log:            commonlog.Named(log, "spire-bridge"),
		metrics:        metrics,
		backoffInitial: initialStreamBackoff,
		backoffMax:     maxStreamBackoff,
		secrets:        make(map[string]*tlsv3.Secret),
		ownBundles:     make(map[string][]byte),
		podBundles:     make(map[string]map[string][]byte),
		podSVIDs:       make(map[string][]byte),
		subscriptions:  make(map[string]podSubscription),
		started:        make(chan struct{}),
	}
}

// Started returns a channel that is closed once the bridge can accept
// subscriptions. Callers re-subscribing stored pods after an agent restart wait
// on it (selecting on their context as well, since the channel never closes if
// Start fails before connecting).
func (b *Bridge) Started() <-chan struct{} {
	return b.started
}

// Start builds the Broker client and serves the agent's own identity. It blocks
// until the context is canceled. Implements controller-runtime Runnable.
//
// Nothing is dialled here and nothing waits for SPIRE: grpc.NewClient is lazy and
// the identity source may still be empty (issue #740). Per-pod subscriptions
// retry on their own backoff until the SPIRE agent answers.
func (b *Bridge) Start(ctx context.Context) error {
	b.ctx = ctx

	client, err := b.newClient(b.socketPath, b.source, b.log)
	if err != nil {
		return fmt.Errorf("creating the SPIFFE Broker API client: %w", err)
	}
	b.client = client
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			b.log.DebugContext(ctx, "failed to close the SPIFFE Broker API client", "error", closeErr)
		}
	}()

	b.log.InfoContext(ctx, "SPIFFE Broker API client ready", "socket", b.socketPath)
	close(b.started)

	// Serve the agent's own node SVID (for node-originated upstream mTLS and the
	// node-health listener) and its trust bundle, and keep both refreshed. At boot
	// the source usually holds neither yet (issue #740); runIdentityRefresh serves
	// them the moment they land, so the failures below are announcements.
	if b.source != nil {
		b.refreshIdentity(ctx, true)
		go b.runIdentityRefresh(ctx)
	}

	<-ctx.Done()
	b.log.InfoContext(ctx, "shutting down SPIRE bridge")
	return nil
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

// SubscribePod starts a Broker subscription for the pod in the given network
// namespace, referenced by its namespace, name and UID. The SPIRE agent resolves
// and attests the pod itself, so every selector its Kubernetes attestor can
// produce (labels, image, sigstore) is usable in the registration entry — and no
// container PID is required. spiffeID is used as the secret name for Envoy.
//
// It is a no-op if the bridge has not been started yet or the netns is already
// subscribed, and it NEVER blocks on the broker: the first subscribe happens on
// the subscription's own goroutine, which retries on the jittered backoff. That
// matters because a reference resolves at request time, so a CNI ADD routinely
// beats the pod into the kubelet's list and gets NotFound. The subscription is
// bound to the bridge's lifetime, not any request context.
func (b *Bridge) SubscribePod(netns, spiffeID string, ref PodRef) error {
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
	b.subscriptions[netns] = podSubscription{cancel: cancel, spiffeID: spiffeID, ref: ref}
	b.subsMu.Unlock()

	b.log.Info("subscribing to SVIDs", "spiffeID", spiffeID, "pod", ref.String())
	go (&subscriptionLoop{
		bridge:   b,
		netns:    netns,
		spiffeID: spiffeID,
		ref:      ref,
		backoff:  b.backoffInitial,
	}).run(subCtx)

	return nil
}

// subscriptionLoop drives one pod's Broker subscription for the bridge's
// lifetime: subscribe, drain, and re-subscribe with backoff whenever the
// subscribe fails or the stream closes (e.g. the SPIRE agent restarts).
//
// The retry state is a struct rather than locals because it spans three
// outcomes that each have to update it consistently — a failed subscribe, a
// successful one, and a stream that ended — and threading four mutable values
// through them as pointers is how the old loop grew past readability.
type subscriptionLoop struct {
	bridge   *Bridge
	netns    string
	spiffeID string
	ref      PodRef

	// backoff is the current re-subscribe delay; degraded records that the last
	// attempt failed, so a success logs and counts a reconnect; unresolvedSince
	// is when the reference first failed to resolve (zero once it has).
	backoff         time.Duration
	degraded        bool
	unresolvedSince time.Time
}

// run loops until ctx is cancelled, or an error the specification says never to
// retry ends the subscription.
func (s *subscriptionLoop) run(ctx context.Context) {
	for {
		ch, err := s.bridge.client.SubscribeX509SVID(ctx, s.ref)
		if err != nil {
			if !s.onSubscribeFailed(ctx, err) {
				return
			}
			continue
		}

		s.onSubscribed(ctx)
		s.bridge.drainSVIDStream(ctx, ch, s.netns, s.spiffeID)
		if ctx.Err() != nil {
			return
		}
		if !s.onStreamEnded(ctx) {
			return
		}
	}
}

// onSubscribeFailed classifies, logs and counts a failed subscribe, then waits.
// It reports whether the loop should try again.
func (s *subscriptionLoop) onSubscribeFailed(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	s.bridge.metrics.streamFailed(ctx, streamSVID)
	retry := s.bridge.reportSubscribeError(ctx, err, s.spiffeID, s.ref, &s.unresolvedSince)
	if !retry.retry {
		return false
	}
	s.degraded = true
	if retry.slow {
		// A policy decision is not going to change in a second; go straight to
		// the slowest backoff rather than hammering the endpoint.
		s.backoff = s.bridge.backoffMax
	}
	return s.wait(ctx)
}

// onSubscribed resets the retry state after a successful subscribe.
func (s *subscriptionLoop) onSubscribed(ctx context.Context) {
	if s.degraded {
		s.bridge.metrics.streamReconnected(ctx, streamSVID)
		s.bridge.log.Info("re-subscribed to SVIDs", "spiffeID", s.spiffeID, "pod", s.ref.String())
	}
	s.degraded = false
	s.unresolvedSince = time.Time{}
	s.backoff = s.bridge.backoffInitial
}

// onStreamEnded handles a stream that closed while the pod is still subscribed
// (e.g. the SPIRE agent restarted) and reports whether to re-subscribe.
//
// Exiting here instead would silently freeze this pod's SVID until it expired.
// The cached SVID keeps serving meanwhile, and the first response on the new
// stream is the current SVID set, so a plain re-subscribe fully resynchronizes.
func (s *subscriptionLoop) onStreamEnded(ctx context.Context) bool {
	s.bridge.metrics.streamFailed(ctx, streamSVID)
	s.degraded = true
	s.bridge.log.Info("SVID subscription stream closed; re-subscribing",
		"spiffeID", s.spiffeID, "pod", s.ref.String(), "backoff", s.backoff)
	return s.wait(ctx)
}

// wait sleeps out the jittered backoff and doubles it, reporting whether the
// full wait elapsed (false = the subscription is shutting down).
func (s *subscriptionLoop) wait(ctx context.Context) bool {
	if !sleepCtx(ctx, jitteredBackoff(s.backoff)) {
		return false
	}
	s.backoff = min(s.backoff*2, s.bridge.backoffMax)
	return true
}

// brokerRetry is how the bridge reacts to a failed subscribe: whether to retry at
// all, and whether to go straight to the slowest backoff.
type brokerRetry struct {
	retry bool
	slow  bool
}

// classifyBrokerError maps a Broker Endpoint gRPC status onto a retry policy.
// The mapping is the one the endpoint specification prescribes (§6 plus Broker
// API §4.8) and is part of proposal 036's decision.
func classifyBrokerError(err error) brokerRetry {
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition:
		// The pod is not in the kubelet's list yet, or it has no registration
		// entry yet. Both resolve on their own; retry on the normal backoff.
		return brokerRetry{retry: true}
	case codes.PermissionDenied:
		// The provider's policy may be non-static, so retry — but slowly: a
		// tight loop against a policy decision is just load.
		return brokerRetry{retry: true, slow: true}
	case codes.Unauthenticated:
		// Credentials are stale. go-spiffe re-reads the SVID from the source on
		// every handshake, so a plain retry IS the refresh.
		return brokerRetry{retry: true}
	case codes.InvalidArgument:
		// A malformed request or a missing security header: a bug in this client,
		// which retrying cannot fix.
		return brokerRetry{}
	default:
		// Unavailable (SPIRE agent down, still initializing, load shedding) and
		// anything unforeseen: exactly today's SPIRE-outage behaviour.
		return brokerRetry{retry: true}
	}
}

// reportSubscribeError logs and counts a failed subscribe and returns the retry
// policy. unresolvedSince carries how long this subscription has been failing
// since its last success, which is the only thing that distinguishes the benign
// cases (the CNI-ADD race; an agent or SPIRE agent that has just restarted) from
// a reference that is never going to resolve or an endpoint that is really gone.
func (b *Bridge) reportSubscribeError(ctx context.Context, err error, spiffeID string, ref PodRef, unresolvedSince *time.Time) brokerRetry {
	retry := classifyBrokerError(err)
	code := status.Code(err)

	switch code {
	case codes.NotFound, codes.FailedPrecondition:
		b.metrics.referenceNotFound(ctx)
		if unresolvedSince.IsZero() {
			*unresolvedSince = time.Now()
		}
		elapsed := time.Since(*unresolvedSince)
		level := slog.LevelInfo
		if elapsed >= referenceUnresolvedWarnAfter {
			level = slog.LevelWarn
		}
		b.log.Log(ctx, level, "the SPIFFE Broker Endpoint has not resolved this pod yet; retrying",
			"spiffeID", spiffeID, "pod", ref.String(), "code", code.String(),
			"elapsed", elapsed.Round(time.Millisecond), "warnAfter", referenceUnresolvedWarnAfter, "error", err)
	case codes.PermissionDenied:
		b.metrics.permissionDenied(ctx)
		b.log.ErrorContext(ctx, "the SPIFFE Broker Endpoint denied this agent the pod's identity; retrying slowly in case the provider's policy is not static",
			"spiffeID", spiffeID, "pod", ref.String(), "error", err)
	case codes.Unauthenticated:
		b.log.ErrorContext(ctx, "the SPIFFE Broker Endpoint could not authenticate this agent; refreshing credentials from the Workload API and retrying",
			"spiffeID", spiffeID, "pod", ref.String(), "error", err)
	case codes.InvalidArgument:
		b.log.ErrorContext(ctx, "the SPIFFE Broker Endpoint rejected the request as malformed; not retrying (this is a bug in the aether agent)",
			"spiffeID", spiffeID, "pod", ref.String(), "error", err)
	default:
		if unresolvedSince.IsZero() {
			*unresolvedSince = time.Now()
		}
		elapsed := time.Since(*unresolvedSince)
		level := slog.LevelWarn
		if elapsed >= brokerUnreachableErrorAfter {
			level = slog.LevelError
		}
		b.log.Log(ctx, level, "subscribing to the pod's X.509 SVIDs failed; retrying",
			"spiffeID", spiffeID, "pod", ref.String(), "code", code.String(),
			"elapsed", elapsed.Round(time.Millisecond), "errorAfter", brokerUnreachableErrorAfter, "error", err)
	}

	return retry
}

// drainSVIDStream reads responses from ch until it is closed or subCtx is
// cancelled.
func (b *Bridge) drainSVIDStream(subCtx context.Context, ch <-chan *brokerpb.SubscribeToX509SVIDResponse, netns, spiffeID string) {
	for {
		select {
		case <-subCtx.Done():
			return
		case resp, ok := <-ch:
			if !ok {
				return
			}
			// Use subCtx (tied to the bridge/subscription lifetime), not the
			// caller's request context: SubscribePod is called synchronously
			// from CmdAdd, whose context is cancelled as soon as it returns —
			// pushing the SVID into the snapshot must outlive that request.
			if handleErr := b.handleSVIDUpdate(subCtx, netns, resp); handleErr != nil {
				b.log.Error("handling SVID update", "error", handleErr, "spiffeID", spiffeID)
			}
		}
	}
}

// Subscribed reports whether a Broker subscription exists for the pod in the
// given network namespace.
func (b *Bridge) Subscribed(netns string) bool {
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	_, ok := b.subscriptions[netns]
	return ok
}

// UnsubscribePod stops the Broker subscription for the pod in the given network
// namespace. The pod's SVID secret is removed only when no other subscribed pod
// shares the same SPIFFE ID (service account), so a rolling restart that briefly
// runs two same-identity pods never drops the live SVID. The pod's contribution
// to the federated-bundle union is dropped too. No-op if the bridge has not been
// started or the netns is not subscribed.
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

	b.mu.Lock()
	delete(b.podSVIDs, netns)
	mutated := false
	if !stillReferenced {
		if _, served := b.secrets[sub.spiffeID]; served {
			delete(b.secrets, sub.spiffeID)
			mutated = true
		}
	}
	if b.setPodBundlesLocked(netns, nil) {
		changed, err := b.rebuildValidationContextsLocked(ctx)
		if err != nil {
			b.log.ErrorContext(ctx, "rebuilding validation contexts after an unsubscribe", "error", err, "netns", netns)
		}
		mutated = mutated || changed
	}
	if mutated {
		b.bumpGenLocked()
	}
	b.mu.Unlock()

	return b.pushSecrets(ctx)
}

// handleSVIDUpdate processes one Broker response: the pod's X.509-SVIDs become
// Envoy TLS-certificate secrets, and the federated bundles it carries become this
// pod's contribution to the served validation contexts.
func (b *Bridge) handleSVIDUpdate(ctx context.Context, netns string, resp *brokerpb.SubscribeToX509SVIDResponse) error {
	// Convert before touching the map so a malformed SVID mid-response leaves
	// the served set untouched rather than half-applied and unpushed.
	svids := resp.GetSvids()
	next := make([]*tlsv3.Secret, 0, len(svids))
	for _, svid := range svids {
		secret, err := SVIDToTLSCertificateSecret(svid)
		if err != nil {
			return fmt.Errorf("converting SVID: %w", err)
		}
		next = append(next, secret)
	}

	var bundleErr error
	b.mu.Lock()
	mutated := false
	for _, secret := range next {
		b.secrets[secret.GetName()] = secret
		mutated = true
	}
	update := b.classifyPodSVIDLocked(netns, next)
	_, hadBundles := b.podBundles[netns]
	bundlesChanged := b.setPodBundlesLocked(netns, resp.GetFederatedBundles())
	if bundlesChanged {
		changed, err := b.rebuildValidationContextsLocked(ctx)
		bundleErr = err
		mutated = mutated || changed
	}
	if mutated {
		b.bumpGenLocked()
	}
	b.mu.Unlock()

	if update != "" {
		b.metrics.svidUpdated(ctx, identityPod, update)
	}
	if bundlesChanged {
		b.metrics.bundleUpdated(ctx, bundleFederated, initialOrRotated(!hadBundles))
	}
	if update == updateRotated {
		// Rare (once per pod per SVID half-life) and the one healthy event worth a
		// line: until this existed, a rotation could only be inferred from Envoy.
		b.log.InfoContext(ctx, "pod SVID rotated", "netns", netns, "spiffeID", next[0].GetName())
	}
	b.log.DebugContext(ctx, "processed SVID update", "svids", len(svids), "federatedBundles", len(resp.GetFederatedBundles()), "update", update)

	if err := b.pushSecrets(ctx); err != nil {
		return err
	}
	return bundleErr
}

// setPodBundlesLocked records a pod's federated bundles and reports whether the
// union's inputs changed. A nil/empty map drops the pod's contribution entirely.
// Callers must hold b.mu.
func (b *Bridge) setPodBundlesLocked(netns string, federated map[string][]byte) bool {
	existing, had := b.podBundles[netns]
	if len(federated) == 0 {
		if !had {
			return false
		}
		delete(b.podBundles, netns)
		return true
	}
	if had && maps.EqualFunc(existing, federated, bytes.Equal) {
		return false
	}
	b.podBundles[netns] = maps.Clone(federated)
	return true
}

// rebuildValidationContextsLocked recomputes the served validation contexts from
// the two bundle inputs (the agent's own Workload API bundle and the union of the
// live pods' federated bundles) and reports whether the served set changed.
// Callers must hold b.mu.
//
// The whole new set is built off to the side and swapped in only once every trust
// domain converted. The previous shape deleted every validation context first and
// re-added them one at a time, so one malformed bundle mid-loop left the map with
// no trust bundles at all — and any other goroutine's next push then published a
// validation-context-less secret set, costing Envoy every peer it could verify
// (issue #772, S11).
func (b *Bridge) rebuildValidationContextsLocked(ctx context.Context) (bool, error) {
	merged := make(map[string][]byte, len(b.ownBundles)+len(b.podBundles))
	for _, perPod := range b.podBundles {
		maps.Copy(merged, perPod)
	}
	// The agent's own Workload API bundle is authoritative for its trust domain:
	// applied last so a peer's federated copy can never shadow it.
	maps.Copy(merged, b.ownBundles)

	next := make(map[string]*tlsv3.Secret, len(merged))
	for trustDomain, der := range merged {
		secret, err := BundleToValidationContextSecret(trustDomain, der)
		if err != nil {
			// Nothing has been mutated yet: the cached bundles keep serving.
			return false, fmt.Errorf("converting bundle for %s: %w", trustDomain, err)
		}
		// Key by the canonical secret name, not the map key: the two differ when
		// the bundle is reported under a bare trust domain, and the store re-keys
		// by name anyway.
		next[secret.GetName()] = secret
	}

	if len(next) == 0 && b.hasValidationContextLocked() {
		// An empty bundle set is never a legitimate instruction to stop verifying
		// peers. Keep what we have and let the next update correct it.
		b.metrics.emptyBundleSkipped(ctx)
		b.log.WarnContext(ctx, "trust bundle inputs went empty; keeping the previously served validation contexts")
		return false, nil
	}

	changed := false
	for name, secret := range b.secrets {
		if _, isValidation := secret.Type.(*tlsv3.Secret_ValidationContext); !isValidation {
			continue
		}
		if _, keep := next[name]; !keep {
			delete(b.secrets, name)
			changed = true
		}
	}
	for name, secret := range next {
		if existing, served := b.secrets[name]; served && validationContextsEqual(existing, secret) {
			continue
		}
		b.secrets[name] = secret
		changed = true
	}
	return changed, nil
}

// hasValidationContextLocked reports whether any trust bundle is currently
// served. Callers must hold b.mu.
func (b *Bridge) hasValidationContextLocked() bool {
	for _, secret := range b.secrets {
		if _, isValidation := secret.Type.(*tlsv3.Secret_ValidationContext); isValidation {
			return true
		}
	}
	return false
}

// validationContextsEqual reports whether two secrets carry the same trusted CA
// bytes, used to skip a no-op snapshot bump when a bundle is re-reported
// unchanged.
func validationContextsEqual(a, b *tlsv3.Secret) bool {
	return bytes.Equal(
		a.GetValidationContext().GetTrustedCa().GetInlineBytes(),
		b.GetValidationContext().GetTrustedCa().GetInlineBytes(),
	)
}

// runIdentityRefresh re-reads and re-serves the agent's own SVID and trust bundle
// whenever the source says either changed, and on a periodic tick as a backstop.
// It returns when the context is cancelled.
//
// The update channel is what makes a LATE first identity cheap: with SPIRE still
// coming up the initial refresh in Start finds nothing, and before issue #740 the
// node identity then waited for the next 30s tick even though the SVID may have
// landed a second later. Sources that do not announce updates fall back to the
// tick alone, exactly as before.
func (b *Bridge) runIdentityRefresh(ctx context.Context) {
	ticker := time.NewTicker(identityRefreshInterval)
	defer ticker.Stop()

	var updated <-chan struct{}
	if src, ok := b.source.(UpdatedSource); ok {
		updated = src.Updated()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-updated:
		}
		b.refreshIdentity(ctx, false)
	}
}

// refreshIdentity re-serves both halves of the agent's own identity. announce
// selects the log level for a miss: at Start a missing identity is expected and
// logged at INFO, afterwards it is routine churn and logged at DEBUG.
func (b *Bridge) refreshIdentity(ctx context.Context, announce bool) {
	if err := b.refreshNodeSVID(ctx); err != nil {
		if announce {
			// Not an error at boot: while SPIRE is still coming up the source
			// holds no SVID yet (issue #740). runIdentityRefresh serves it the
			// moment it lands, so this is an announcement, not a failure.
			b.log.InfoContext(ctx, "node SVID not available yet; serving it as soon as SPIRE issues one", "error", err)
		} else {
			b.log.DebugContext(ctx, "refreshing node SVID", "error", err)
		}
	}
	if err := b.refreshWorkloadBundle(ctx); err != nil {
		if announce {
			b.log.InfoContext(ctx, "trust bundle not available yet; serving validation contexts as soon as SPIRE issues one", "error", err)
		} else {
			b.log.DebugContext(ctx, "refreshing the Workload API trust bundle", "error", err)
		}
	}
}

// refreshNodeSVID reads the current node SVID from the Workload API source and,
// if it changed, updates the cached secret and pushes it. It is a no-op when the
// source is unset.
func (b *Bridge) refreshNodeSVID(ctx context.Context) error {
	if b.source == nil {
		return nil
	}

	svid, err := b.source.GetX509SVID()
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
	b.bumpGenLocked()
	firstServe := b.nodeSpiffeID == ""
	b.nodeSpiffeID = secret.GetName()
	b.mu.Unlock()

	b.metrics.svidUpdated(ctx, identityNode, initialOrRotated(firstServe))
	if !firstServe {
		b.log.InfoContext(ctx, "node SVID rotated", "spiffeID", secret.GetName())
	}

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

// refreshWorkloadBundle reads the agent's own trust bundle from the Workload API
// source and, if it changed, rebuilds and republishes the validation contexts.
//
// This is what replaces the delegated API's node-wide bundle stream: the Broker
// API has no equivalent, because SubscribeToX509Bundles also needs a workload
// reference and a node with no managed pods has none. The agent's own bundle is
// available as soon as SPIRE attests the agent itself — strictly earlier than any
// pod's — so nothing is lost.
func (b *Bridge) refreshWorkloadBundle(ctx context.Context) error {
	if b.source == nil {
		return nil
	}

	svid, err := b.source.GetX509SVID()
	if err != nil {
		return fmt.Errorf("fetching this agent's SVID to resolve its trust domain: %w", err)
	}
	td := svid.ID.TrustDomain()

	bundle, err := b.source.GetX509BundleForTrustDomain(td)
	if err != nil {
		return fmt.Errorf("fetching the X.509 bundle for trust domain %q: %w", td.Name(), err)
	}

	var der []byte
	for _, authority := range bundle.X509Authorities() {
		der = append(der, authority.Raw...)
	}
	if len(der) == 0 {
		return fmt.Errorf("the X.509 bundle for trust domain %q carries no authorities", td.Name())
	}

	b.mu.Lock()
	if existing, ok := b.ownBundles[td.IDString()]; ok && bytes.Equal(existing, der) {
		b.mu.Unlock()
		return nil // unchanged; avoid a no-op snapshot bump
	}
	firstBundle := len(b.ownBundles) == 0
	b.ownBundles = map[string][]byte{td.IDString(): der}
	changed, rebuildErr := b.rebuildValidationContextsLocked(ctx)
	if changed {
		b.bumpGenLocked()
	}
	b.mu.Unlock()

	b.metrics.bundleUpdated(ctx, bundleOwn, initialOrRotated(firstBundle))
	if !firstBundle {
		b.log.InfoContext(ctx, "Workload API trust bundle changed", "trustDomain", td.Name(), "authorities", len(bundle.X509Authorities()))
	}
	if rebuildErr != nil {
		return rebuildErr
	}
	b.log.DebugContext(ctx, "served the Workload API trust bundle", "trustDomain", td.Name())
	return b.pushSecrets(ctx)
}

// classifyPodSVIDLocked compares the first SVID of a Broker response with the one
// last served for this pod and records the new one. It returns "" for a response
// that carried no SVID (a federated-bundle-only update). Callers must hold b.mu.
func (b *Bridge) classifyPodSVIDLocked(netns string, secrets []*tlsv3.Secret) string {
	if len(secrets) == 0 {
		return ""
	}
	chain := secrets[0].GetTlsCertificate().GetCertificateChain().GetInlineBytes()
	previous, seen := b.podSVIDs[netns]
	b.podSVIDs[netns] = chain
	switch {
	case !seen:
		return updateInitial
	case bytes.Equal(previous, chain):
		return updateUnchanged
	default:
		return updateRotated
	}
}

// initialOrRotated names an update by whether anything was served before it.
func initialOrRotated(first bool) string {
	if first {
		return updateInitial
	}
	return updateRotated
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
//
// Publication is monotonic. pushMu is held across the snapshot AND the store
// call, so concurrent pushers publish in the order they acquire it and each one
// carries the newest secret set at the moment it built — never an older one.
// The generation stamp turns that into a checked invariant and lets overlapping
// wakes collapse: a pusher that finds the store already holding its generation
// returns without a redundant SetSecrets, which would otherwise regenerate the
// whole node snapshot for a set the proxy already has.
func (b *Bridge) pushSecrets(ctx context.Context) error {
	b.pushMu.Lock()
	defer b.pushMu.Unlock()

	// Read the generation and the map together: they must come from one
	// critical section or the stamp would not describe the slice.
	b.mu.RLock()
	gen := b.gen
	secrets := make([]*tlsv3.Secret, 0, len(b.secrets))
	for _, s := range b.secrets {
		secrets = append(secrets, s)
	}
	b.mu.RUnlock()

	switch {
	case gen == b.publishedGen:
		// An overlapping push already carried this exact state.
		b.log.DebugContext(ctx, "skipping SDS push; snapshot already holds this generation", "generation", gen)
		return nil
	case gen < b.publishedGen:
		// Unreachable while pushMu covers the build: a build under the lock
		// cannot observe a state older than what the lock's previous holder
		// published. Kept as a fail-safe so a future caller that builds outside
		// the lock is caught by a counter instead of silently regressing SDS.
		b.metrics.stalePushRejected(ctx)
		b.log.WarnContext(ctx, "rejecting stale SDS push; snapshot holds a newer generation",
			"generation", gen, "published", b.publishedGen)
		return nil
	}

	if err := b.store.SetSecrets(ctx, secrets); err != nil {
		// Leave publishedGen alone: the next push retries this generation.
		return err
	}
	b.publishedGen = gen
	return nil
}

// bumpGenLocked records that the secret set changed. Callers must hold b.mu for
// writing.
func (b *Bridge) bumpGenLocked() {
	b.gen++
}
