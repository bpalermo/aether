package meshconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"aethermesh.dev/common/spire"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCABundleCallTimeout bounds the read and the write of ONE webhook
	// configuration. Both are single-object calls an apiserver answers in
	// milliseconds, so a call that is still out after this long is failing, and
	// is reported and retried as a failure (issue #1431).
	DefaultCABundleCallTimeout = 15 * time.Second
	// DefaultCABundleRetryMin and DefaultCABundleRetryMax bound the backoff
	// between retries while a webhook configuration cannot be injected. The
	// ceiling is also how long the injector takes, at worst, to notice that a
	// missing permission has been granted.
	DefaultCABundleRetryMin = time.Second
	DefaultCABundleRetryMax = time.Minute

	// caBundleMeterName identifies this instrumentation scope in metric backends.
	caBundleMeterName = "aether/controller-webhook"
	// CABundleInjectionFailuresMetric counts failed injection attempts, one per
	// webhook configuration and attempt, by `kind` (validating, mutating) and
	// `reason` (trust_bundle, get, update). Expected to stay 0.
	CABundleInjectionFailuresMetric = "aether.controller.webhook.cabundle_injection_failures"

	// The messages an operator greps for; the runbook quotes them.
	msgInjectionFailed   = "webhook caBundle injection failed"
	msgInjectionDeferred = "webhook caBundle injection deferred until this workload has an SVID"
)

// Failure reasons, the `reason` attribute of CABundleInjectionFailuresMetric.
const (
	reasonTrustBundle = "trust_bundle"
	reasonGet         = "get"
	reasonUpdate      = "update"
)

// ClientSource is the part of a controller-runtime manager the injector takes
// its clients from. The manager satisfies it.
type ClientSource interface {
	// GetClient is the manager's client: reads come from the informer cache.
	GetClient() client.Client
	// GetAPIReader reads from the apiserver directly.
	GetAPIReader() client.Reader
}

// CABundleInjector keeps the webhooks' caBundle in sync with the SPIRE trust
// bundle. The webhooks are served with a SPIRE X.509 SVID, so the
// kube-apiserver must trust the SPIRE CA — this runnable writes the current
// bundle into the ValidatingWebhookConfiguration (and the
// MutatingWebhookConfiguration, when one is named) on startup and on every SVID
// rotation. It runs on the leader only (a cluster-wide single writer).
//
// Two properties are load-bearing (issue #1431):
//
//   - It cannot hang. Every read goes to the apiserver through Reader, never
//     through the manager's informer cache, and each configuration's read and
//     write share one bounded context. A cached read waits, with no deadline of
//     its own, for an informer that may never sync — a missing list permission
//     is enough — and the loop that should have reported the failure never came
//     back.
//   - The configurations are independent. Each is read, written, reported and
//     retried on its own, so one that keeps failing never stops the other from
//     following a trust-bundle rotation.
type CABundleInjector struct {
	// Reader reads the webhook configurations. It MUST be uncached (the
	// manager's GetAPIReader): see the type comment. Two single-object reads per
	// pass cost nothing next to a cluster-wide informer on both resources, and
	// need only the `get` permission.
	Reader client.Reader
	// Writer updates them.
	Writer client.Writer
	// Source is this workload's SVID source. It is the narrow SVIDSource interface
	// rather than a *spire.Source because the controller's source may still be
	// waiting for its first SVID when this runnable starts (issue #740): the
	// initial injection is then deferred (INFO, not ERROR) and performed on the
	// Updated() wake that WaitingSource fires when the SVID lands.
	Source            spire.SVIDSource
	WebhookConfigName string
	// MutatingWebhookConfigName is the pod-mutating MutatingWebhookConfiguration
	// to keep in sync too; empty skips it.
	MutatingWebhookConfigName string
	Log                       *slog.Logger

	// CallTimeout, RetryMin and RetryMax override the Default* constants above;
	// zero keeps the default.
	CallTimeout time.Duration
	RetryMin    time.Duration
	RetryMax    time.Duration

	failures metric.Int64Counter
}

// NewCABundleInjector builds the injector from a manager's clients: reads go
// through the manager's API reader, writes through its client.
func NewCABundleInjector(m ClientSource, source spire.SVIDSource, webhookConfigName, mutatingWebhookConfigName string, log *slog.Logger) *CABundleInjector {
	return &CABundleInjector{
		Reader:                    m.GetAPIReader(),
		Writer:                    m.GetClient(),
		Source:                    source,
		WebhookConfigName:         webhookConfigName,
		MutatingWebhookConfigName: mutatingWebhookConfigName,
		Log:                       log,
	}
}

// NeedLeaderElection keeps caBundle writes to a single replica.
func (i *CABundleInjector) NeedLeaderElection() bool { return true }

// webhookTarget is one webhook configuration the injector maintains.
type webhookTarget struct {
	// kind is the `kind` attribute of the failure counter.
	kind string
	// objectKind is the Kubernetes kind, for logs and errors.
	objectKind string
	name       string
	newObject  func() client.Object
	// injectedMsg is logged when the bundle was written.
	injectedMsg string
}

// targets lists the configurations this injector maintains, validating first.
func (i *CABundleInjector) targets() []webhookTarget {
	out := []webhookTarget{{
		kind:        "validating",
		objectKind:  "ValidatingWebhookConfiguration",
		name:        i.WebhookConfigName,
		newObject:   func() client.Object { return &admissionregistrationv1.ValidatingWebhookConfiguration{} },
		injectedMsg: "injected SPIRE trust bundle into webhook caBundle",
	}}
	if i.MutatingWebhookConfigName != "" {
		out = append(out, webhookTarget{
			kind:        "mutating",
			objectKind:  "MutatingWebhookConfiguration",
			name:        i.MutatingWebhookConfigName,
			newObject:   func() client.Object { return &admissionregistrationv1.MutatingWebhookConfiguration{} },
			injectedMsg: "injected SPIRE trust bundle into mutating webhook caBundle",
		})
	}
	return out
}

// injectionFailure is one configuration that could not be injected in a pass.
type injectionFailure struct {
	target webhookTarget
	reason string
	err    error
}

// Start injects the bundle once, then re-injects whenever the SPIRE source
// reports a rotation, until the context is cancelled. A pass in which a
// configuration failed is repeated with exponential backoff, so a failure never
// has to wait for the next rotation to be retried.
//
// It never fails startup: failurePolicy=Ignore means an un-injected webhook
// fails open, and the retry keeps trying.
func (i *CABundleInjector) Start(ctx context.Context) error {
	i.failures = newInjectionFailuresCounter(ctx, i.Log)

	var (
		delay    time.Duration
		deferred bool
	)
	for {
		failures, noSVID := i.inject(ctx)
		if ctx.Err() != nil {
			// Shutting down: whatever failed, failed because of the cancellation.
			return nil
		}
		if noSVID && !deferred {
			// Not a failure, and not something an operator can act on: SPIRE has not
			// issued this workload's first SVID yet, so there is no trust bundle to
			// inject (issue #740). The wait itself is announced, once per attempt, by
			// the identity source. WaitingSource fires Updated() when the first SVID
			// LANDS as well as on every rotation after it, so the select below performs
			// the initial injection — no polling for it here.
			deferred = true
			i.Log.InfoContext(ctx, msgInjectionDeferred, "webhookConfig", i.WebhookConfigName)
		}

		if len(failures) == 0 {
			delay = 0
		} else {
			delay = i.nextDelay(delay)
			i.report(ctx, failures, delay)
		}
		if !i.wait(ctx, delay) {
			return nil
		}
	}
}

// wait blocks until the next pass is due: a trust-bundle change, or retryIn
// elapsing when the last pass left a failure behind (zero means no retry is
// pending). It returns false when the context is cancelled instead.
func (i *CABundleInjector) wait(ctx context.Context, retryIn time.Duration) bool {
	var retry <-chan time.Time
	if retryIn > 0 {
		timer := time.NewTimer(retryIn)
		defer timer.Stop()
		retry = timer.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-i.Source.Updated():
		return true
	case <-retry:
		return true
	}
}

// nextDelay doubles the retry delay from RetryMin up to RetryMax.
func (i *CABundleInjector) nextDelay(prev time.Duration) time.Duration {
	lo, hi := orDefault(i.RetryMin, DefaultCABundleRetryMin), orDefault(i.RetryMax, DefaultCABundleRetryMax)
	if prev <= 0 {
		return lo
	}
	return min(prev*2, hi)
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// report logs and counts each configuration that failed in a pass. Every
// failure names its own object, so a log search for one webhook's name finds
// the reason that webhook is not being called.
func (i *CABundleInjector) report(ctx context.Context, failures []injectionFailure, retryIn time.Duration) {
	for _, f := range failures {
		// Counted before it is logged, so a line in the log is never ahead of the
		// counter it explains.
		if i.failures != nil {
			i.failures.Add(ctx, 1, metric.WithAttributes(
				attribute.String("kind", f.target.kind),
				attribute.String("reason", f.reason)))
		}
		i.Log.ErrorContext(ctx, msgInjectionFailed,
			"kind", f.target.objectKind,
			"webhookConfig", f.target.name,
			"reason", f.reason,
			"retryIn", retryIn.String(),
			"error", f.err)
	}
}

// inject runs one pass: it sets the current SPIRE trust bundle as the caBundle
// of every webhook in every configuration this injector maintains, and returns
// the configurations that failed. noSVID reports that there is no trust bundle
// to inject yet, which is a wait and not a failure.
func (i *CABundleInjector) inject(ctx context.Context) (failures []injectionFailure, noSVID bool) {
	targets := i.targets()

	bundle, err := spire.TrustBundlePEM(i.Source)
	if errors.Is(err, spire.ErrNoSVIDYet) {
		return nil, true
	}
	if err != nil {
		// No bundle means no configuration can be injected: each one failed.
		for _, target := range targets {
			failures = append(failures, injectionFailure{target: target, reason: reasonTrustBundle, err: err})
		}
		return failures, false
	}

	for _, target := range targets {
		if reason, err := i.injectOne(ctx, target, bundle); err != nil {
			failures = append(failures, injectionFailure{target: target, reason: reason, err: err})
		}
	}
	return failures, false
}

// injectOne brings one configuration's caBundle up to date via
// read-modify-write (Update), NOT Server-Side Apply: the `webhooks` list is
// atomic for SSA, so a partial apply would replace the whole entry and drop
// Helm-owned required fields (sideEffects, admissionReviewVersions). This is
// the standard caBundle-injection pattern (cf. cert-manager's cainjector).
//
// The read and the write share one deadline, so the call returns — with an
// error naming the object — whatever the apiserver or the client does.
func (i *CABundleInjector) injectOne(ctx context.Context, target webhookTarget, bundle []byte) (reason string, err error) {
	ctx, cancel := context.WithTimeout(ctx, orDefault(i.CallTimeout, DefaultCABundleCallTimeout))
	defer cancel()

	obj := target.newObject()
	if err := i.Reader.Get(ctx, types.NamespacedName{Name: target.name}, obj); err != nil {
		return reasonGet, fmt.Errorf("get %s %q: %w", target.objectKind, target.name, err)
	}
	if !setCABundle(obj, bundle) {
		return "", nil
	}
	if err := i.Writer.Update(ctx, obj); err != nil {
		return reasonUpdate, fmt.Errorf("update %s %q: %w", target.objectKind, target.name, err)
	}
	i.Log.InfoContext(ctx, target.injectedMsg, "webhookConfig", target.name)
	return "", nil
}

// setCABundle sets bundle on every webhook of a webhook configuration and
// reports whether anything changed.
func setCABundle(obj client.Object, bundle []byte) bool {
	changed := false
	set := func(cc *admissionregistrationv1.WebhookClientConfig) {
		if !bytes.Equal(cc.CABundle, bundle) {
			cc.CABundle = bundle
			changed = true
		}
	}
	switch cfg := obj.(type) {
	case *admissionregistrationv1.ValidatingWebhookConfiguration:
		for idx := range cfg.Webhooks {
			set(&cfg.Webhooks[idx].ClientConfig)
		}
	case *admissionregistrationv1.MutatingWebhookConfiguration:
		for idx := range cfg.Webhooks {
			set(&cfg.Webhooks[idx].ClientConfig)
		}
	}
	return changed
}

// newInjectionFailuresCounter registers the failure counter on the global meter
// provider. Telemetry is best-effort: a registration failure costs the counter,
// never the injector.
func newInjectionFailuresCounter(ctx context.Context, log *slog.Logger) metric.Int64Counter {
	counter, err := otel.Meter(caBundleMeterName).Int64Counter(CABundleInjectionFailuresMetric,
		metric.WithDescription("Failed attempts to write the SPIRE trust bundle into a webhook configuration's caBundle, by kind and reason; expected to stay 0"))
	if err != nil {
		log.WarnContext(ctx, "failed to register the webhook caBundle failure counter; continuing without it", "error", err)
		return nil
	}
	return counter
}
