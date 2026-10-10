package meshconfig

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	// eventually bounds every wait for something the injector must do; settle is
	// how long a thing that must NOT happen is watched for.
	eventually = 30 * time.Second
	settle     = 300 * time.Millisecond
	tick       = 10 * time.Millisecond

	// Test backoff: fast enough that a retry is a few ticks away.
	testRetryMin = 10 * time.Millisecond
	testRetryMax = 50 * time.Millisecond
)

// rotatingIdentity is an SVID source whose trust bundle the test can rotate.
type rotatingIdentity struct {
	*spiretest.Identity
	t  *testing.T
	td spiffeid.TrustDomain
}

// newRotatingIdentity returns a source holding an SVID, plus the PEM of its
// trust bundle.
func newRotatingIdentity(t *testing.T) (*rotatingIdentity, []byte) {
	t.Helper()

	td := spiffeid.RequireTrustDomainFromString(spiretest.TrustDomain)
	ca := spiretest.NewCA(t)
	pem, err := ca.Bundle(td).Marshal()
	require.NoError(t, err)
	return &rotatingIdentity{Identity: ca.Identity(t, testSpiffeID), t: t, td: td}, pem
}

// rotate replaces the SVID and the trust bundle with ones from a new CA, wakes
// the injector the way a SPIRE rotation does, and returns the new bundle's PEM.
func (r *rotatingIdentity) rotate() []byte {
	r.t.Helper()

	ca := spiretest.NewCA(r.t)
	pem, err := ca.Bundle(r.td).Marshal()
	require.NoError(r.t, err)
	r.Arrive(ca.SVID(r.t, testSpiffeID), ca.Bundle(r.td))
	return pem
}

// testLogger returns a JSON logger and the buffer it writes to.
func testLogger() (*slog.Logger, *lockedBuffer) {
	logs := &lockedBuffer{}
	return slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), logs
}

// failureFor returns the first `webhook caBundle injection failed` record that
// names the given webhook configuration, or nil.
func (b *lockedBuffer) failureFor(t *testing.T, webhookConfig string) map[string]any {
	t.Helper()

	for _, rec := range b.records(t) {
		if rec["msg"] == msgInjectionFailed && rec["webhookConfig"] == webhookConfig {
			return rec
		}
	}
	return nil
}

// startInjector runs the injector until the test ends and fails the test if it
// does not return once cancelled, or returns an error.
func startInjector(ctx context.Context, t *testing.T, injector *CABundleInjector) {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- injector.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err, "the injector must never fail the manager")
		case <-time.After(eventually):
			t.Error("the injector did not return after cancellation")
		}
	})
}

// installTestMeterProvider points the global meter provider at a manual reader
// for the duration of the test.
func installTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

// injectionFailures returns the failure counter's value for one kind and reason.
func injectionFailures(t *testing.T, reader *sdkmetric.ManualReader, kind, reason string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != CABundleInjectionFailuresMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s must be an int64 counter", m.Name)
			for _, dp := range sum.DataPoints {
				k, _ := dp.Attributes.Value(attribute.Key("kind"))
				r, _ := dp.Attributes.Value(attribute.Key("reason"))
				if k.AsString() == kind && r.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// TestCABundleInjectorNeverWaitsOnAnInformer is the regression test for issue
// #1431, on the client stack the controller-runtime manager builds: a real
// informer cache, the cache-backed client and the uncached API reader, against
// an apiserver that answers 403 for every verb on
// mutatingwebhookconfigurations (the RBAC gap of #1411).
//
// Before the fix the injector read through the cache-backed client. Its Get on
// the mutating configuration started an informer that could never list, and
// waited for it with no deadline: the injector logged nothing, never looked at
// the SPIRE source again — so the VALIDATING webhook kept its pre-rotation
// bundle — and the unsynced informer failed the leader's `cache-sync`
// readiness check.
func TestCABundleInjectorNeverWaitsOnAnInformer(t *testing.T) {
	metrics := installTestMeterProvider(t)
	api := startFakeAPIServer(t)
	api.forbid(mutatingResource, "get", "list", "watch", "update")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clients := api.newManagerClients(ctx, t)

	log, logs := testLogger()
	identity, bundle := newRotatingIdentity(t)

	injector := NewCABundleInjector(clients, identity, testWebhookConfigName, testMutatingWebhookConfigName, log)
	injector.RetryMin, injector.RetryMax = testRetryMin, testRetryMax
	startInjector(ctx, t, injector)

	// The validating webhook gets its bundle, and the mutating one's failure is
	// reported: by name, with the apiserver's reason.
	require.Eventually(t, func() bool { return bytes.Equal(api.validatingBundle(), bundle) },
		eventually, tick, "the validating webhook must be injected; logs:\n%s", logs.String())
	require.Eventually(t, func() bool { return logs.failureFor(t, testMutatingWebhookConfigName) != nil },
		eventually, tick, "the mutating webhook's failure must be logged; logs:\n%s", logs.String())
	failure := logs.failureFor(t, testMutatingWebhookConfigName)
	assert.Equal(t, "ERROR", failure["level"])
	assert.Equal(t, "MutatingWebhookConfiguration", failure["kind"])
	assert.Equal(t, reasonGet, failure["reason"])
	assert.Contains(t, failure["error"], "forbidden")
	assert.NotEmpty(t, failure["retryIn"], "the line says when the next attempt is")
	assert.Nil(t, logs.failureFor(t, testWebhookConfigName), "the validating webhook did not fail")
	assert.Positive(t, injectionFailures(t, metrics, "mutating", reasonGet), "the failure must be counted")
	assert.Zero(t, injectionFailures(t, metrics, "validating", reasonGet))

	// A trust-bundle rotation reaches the validating webhook while the mutating
	// one is still failing.
	rotated := identity.rotate()
	require.Eventually(t, func() bool { return bytes.Equal(api.validatingBundle(), rotated) },
		eventually, tick, "a rotation must refresh the validating webhook while the mutating one fails; logs:\n%s", logs.String())
	assert.Empty(t, api.mutatingBundle(), "the mutating webhook is still forbidden")

	// Readiness: the injector created no informer, so the manager's cache is as
	// synced as it was — the leader stays in the webhook Service.
	syncCtx, syncCancel := context.WithTimeout(ctx, 2*time.Second)
	defer syncCancel()
	assert.True(t, clients.cache.WaitForCacheSync(syncCtx),
		"a webhook configuration the controller cannot read must not fail the cache-sync readiness check")
	for _, resource := range []string{validatingResource, mutatingResource} {
		assert.Zero(t, api.served(resource, "list"), "no informer may be started on %s", resource)
		assert.Zero(t, api.served(resource, "watch"), "no informer may be started on %s", resource)
	}

	// The permission appears: the retry — no rotation, no restart — injects the
	// current bundle.
	api.allow(mutatingResource, "get", "list", "watch", "update")
	require.Eventually(t, func() bool { return bytes.Equal(api.mutatingBundle(), rotated) },
		eventually, tick, "the mutating webhook must be injected once the permission exists; logs:\n%s", logs.String())
	require.Eventually(t, func() bool {
		return logs.level(t, "injected SPIRE trust bundle into mutating webhook caBundle") == "INFO"
	}, eventually, tick, "the recovery must be announced; logs:\n%s", logs.String())
}

// TestCABundleInjectorNeedsOnlyGetAndUpdate pins what the injector asks of
// RBAC: with `list` and `watch` refused on both resources — which no informer
// survives — both webhooks are injected and follow a rotation. It also sends
// nothing but a get and an update: the fake server answers any other request
// (a PATCH, a create, a delete) with 405 and counts it as "unsupported". That
// is what lets the chart grant `get` and `update` alone (#1456).
func TestCABundleInjectorNeedsOnlyGetAndUpdate(t *testing.T) {
	api := startFakeAPIServer(t)
	api.forbid(validatingResource, "list", "watch")
	api.forbid(mutatingResource, "list", "watch")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clients := api.newManagerClients(ctx, t)

	log, logs := testLogger()
	identity, bundle := newRotatingIdentity(t)
	injector := NewCABundleInjector(clients, identity, testWebhookConfigName, testMutatingWebhookConfigName, log)
	startInjector(ctx, t, injector)

	require.Eventually(t, func() bool {
		return bytes.Equal(api.validatingBundle(), bundle) && bytes.Equal(api.mutatingBundle(), bundle)
	}, eventually, tick, "both webhooks must be injected with get and update alone; logs:\n%s", logs.String())

	rotated := identity.rotate()
	require.Eventually(t, func() bool {
		return bytes.Equal(api.validatingBundle(), rotated) && bytes.Equal(api.mutatingBundle(), rotated)
	}, eventually, tick, "both webhooks must follow a rotation; logs:\n%s", logs.String())
	assert.Empty(t, logs.level(t, msgInjectionFailed))
	for _, resource := range []string{validatingResource, mutatingResource} {
		assert.Positive(t, api.served(resource, "get"), "%s is read with a get", resource)
		assert.Positive(t, api.served(resource, "update"), "%s is written with an update", resource)
		assert.Zero(t, api.served(resource, "unsupported"), "no patch (or any verb but get and update) may be sent to %s", resource)
	}
}

// newWebhookPair returns a fake client holding an un-injected validating and
// mutating webhook configuration, with the given interceptors.
func newWebhookPair(t *testing.T, funcs interceptor.Funcs) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(funcs).WithObjects(
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: testWebhookConfigName},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{Name: "meshconfig.aether.io", SideEffects: ptr(admissionregistrationv1.SideEffectClassNone), AdmissionReviewVersions: []string{"v1"}},
				{Name: "httproute.aether.io", SideEffects: ptr(admissionregistrationv1.SideEffectClassNone), AdmissionReviewVersions: []string{"v1"}},
			},
		},
		&admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: testMutatingWebhookConfigName},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				{Name: "pod.aether.io", SideEffects: ptr(admissionregistrationv1.SideEffectClassNone), AdmissionReviewVersions: []string{"v1"}},
			},
		},
	).Build()
}

// validatingBundles reads the caBundle of every validating webhook entry.
func validatingBundles(t *testing.T, c client.Client) [][]byte {
	t.Helper()

	var cfg admissionregistrationv1.ValidatingWebhookConfiguration
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: testWebhookConfigName}, &cfg))
	out := make([][]byte, 0, len(cfg.Webhooks))
	for _, w := range cfg.Webhooks {
		out = append(out, w.ClientConfig.CABundle)
	}
	return out
}

// mutatingBundleOf reads the caBundle of the mutating webhook.
func mutatingBundleOf(t *testing.T, c client.Client) []byte {
	t.Helper()

	var cfg admissionregistrationv1.MutatingWebhookConfiguration
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: testMutatingWebhookConfigName}, &cfg))
	return cfg.Webhooks[0].ClientConfig.CABundle
}

func allEqual(bundles [][]byte, want []byte) bool {
	for _, b := range bundles {
		if !bytes.Equal(b, want) {
			return false
		}
	}
	return len(bundles) > 0
}

// TestCABundleInjectorBoundsAReadThatNeverAnswers covers every other way a read
// can fail to come back (a wedged connection, a reader that waits on something):
// the call is bounded, so the failure is reported and the validating webhook
// still follows a rotation.
func TestCABundleInjectorBoundsAReadThatNeverAnswers(t *testing.T) {
	var blocked atomic.Int64
	c := newWebhookPair(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*admissionregistrationv1.MutatingWebhookConfiguration); ok {
				// What a cached read of an unsyncable informer does: wait for the
				// caller's context, and nothing else.
				blocked.Add(1)
				<-ctx.Done()
				return ctx.Err()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	log, logs := testLogger()
	identity, bundle := newRotatingIdentity(t)
	injector := &CABundleInjector{
		Reader: c, Writer: c, Source: identity, Log: log,
		WebhookConfigName:         testWebhookConfigName,
		MutatingWebhookConfigName: testMutatingWebhookConfigName,
		CallTimeout:               50 * time.Millisecond,
		RetryMin:                  testRetryMin,
		RetryMax:                  testRetryMax,
	}
	startInjector(t.Context(), t, injector)

	require.Eventually(t, func() bool { return logs.failureFor(t, testMutatingWebhookConfigName) != nil },
		eventually, tick, "a read that never answers must be reported, not waited for; logs:\n%s", logs.String())
	assert.Contains(t, logs.failureFor(t, testMutatingWebhookConfigName)["error"], context.DeadlineExceeded.Error())
	assert.True(t, allEqual(validatingBundles(t, c), bundle), "every validating webhook entry gets the bundle")

	rotated := identity.rotate()
	require.Eventually(t, func() bool { return allEqual(validatingBundles(t, c), rotated) },
		eventually, tick, "the validating webhook must follow a rotation while the mutating read hangs; logs:\n%s", logs.String())
	require.Eventually(t, func() bool { return blocked.Load() > 1 },
		eventually, tick, "the hanging read must be retried")
}

// TestCABundleInjectorAValidatingFailureDoesNotStopTheMutatingWebhook is the
// independence in the other direction: the old injector returned on the first
// error, so a validating configuration that could not be written left the
// mutating one un-injected. It also shows the retry: the write starts working
// and the bundle lands with no rotation to trigger it.
func TestCABundleInjectorAValidatingFailureDoesNotStopTheMutatingWebhook(t *testing.T) {
	metrics := installTestMeterProvider(t)
	var refuse atomic.Bool
	refuse.Store(true)
	c := newWebhookPair(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*admissionregistrationv1.ValidatingWebhookConfiguration); ok && refuse.Load() {
				return apierrors.NewConflict(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: validatingResource},
					obj.GetName(), errors.New("the object has been modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})

	log, logs := testLogger()
	identity, bundle := newRotatingIdentity(t)
	injector := &CABundleInjector{
		Reader: c, Writer: c, Source: identity, Log: log,
		WebhookConfigName:         testWebhookConfigName,
		MutatingWebhookConfigName: testMutatingWebhookConfigName,
		RetryMin:                  testRetryMin,
		RetryMax:                  testRetryMax,
	}
	startInjector(t.Context(), t, injector)

	require.Eventually(t, func() bool { return bytes.Equal(mutatingBundleOf(t, c), bundle) },
		eventually, tick, "the mutating webhook must be injected although the validating one fails; logs:\n%s", logs.String())
	require.Eventually(t, func() bool { return logs.failureFor(t, testWebhookConfigName) != nil },
		eventually, tick, "logs:\n%s", logs.String())
	failure := logs.failureFor(t, testWebhookConfigName)
	assert.Equal(t, "ValidatingWebhookConfiguration", failure["kind"])
	assert.Equal(t, reasonUpdate, failure["reason"])
	assert.Positive(t, injectionFailures(t, metrics, "validating", reasonUpdate))
	assert.Nil(t, logs.failureFor(t, testMutatingWebhookConfigName))

	// No rotation from here on: only the backoff retry can deliver the bundle.
	refuse.Store(false)
	require.Eventually(t, func() bool { return allEqual(validatingBundles(t, c), bundle) },
		eventually, tick, "a failed webhook must be retried without waiting for a rotation; logs:\n%s", logs.String())
}

// TestCABundleInjectorValidatingOnly is the configuration the chart renders
// when no pod-mutating webhook is enabled: one object, and the mutating
// resource is never touched.
func TestCABundleInjectorValidatingOnly(t *testing.T) {
	var mutatingReads atomic.Int64
	c := newWebhookPair(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*admissionregistrationv1.MutatingWebhookConfiguration); ok {
				mutatingReads.Add(1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	log, logs := testLogger()
	identity, bundle := newRotatingIdentity(t)
	injector := &CABundleInjector{
		Reader: c, Writer: c, Source: identity, Log: log,
		WebhookConfigName: testWebhookConfigName,
	}
	startInjector(t.Context(), t, injector)

	require.Eventually(t, func() bool { return allEqual(validatingBundles(t, c), bundle) },
		eventually, tick, "logs:\n%s", logs.String())
	rotated := identity.rotate()
	require.Eventually(t, func() bool { return allEqual(validatingBundles(t, c), rotated) },
		eventually, tick, "logs:\n%s", logs.String())

	assert.Zero(t, mutatingReads.Load(), "an unnamed mutating configuration is never read")
	assert.Empty(t, logs.level(t, msgInjectionFailed))
}

// TestCABundleInjectorRetriesAnUnreadableTrustBundle covers a source that holds
// an SVID but cannot produce the bundle for its trust domain: that is a
// failure of every configuration (unlike a missing SVID, which is a wait), and
// it is reported per object and retried.
func TestCABundleInjectorRetriesAnUnreadableTrustBundle(t *testing.T) {
	metrics := installTestMeterProvider(t)
	c := newWebhookPair(t, interceptor.Funcs{})

	td := spiffeid.RequireTrustDomainFromString(spiretest.TrustDomain)
	ca := spiretest.NewCA(t)
	identity := spiretest.NewPendingIdentity()
	identity.Arrive(ca.SVID(t, testSpiffeID)) // an SVID, and no bundle at all

	log, logs := testLogger()
	injector := &CABundleInjector{
		Reader: c, Writer: c, Source: identity, Log: log,
		WebhookConfigName:         testWebhookConfigName,
		MutatingWebhookConfigName: testMutatingWebhookConfigName,
		RetryMin:                  testRetryMin,
		RetryMax:                  testRetryMax,
	}
	startInjector(t.Context(), t, injector)

	for _, name := range []string{testWebhookConfigName, testMutatingWebhookConfigName} {
		require.Eventually(t, func() bool { return logs.failureFor(t, name) != nil },
			eventually, tick, "logs:\n%s", logs.String())
		assert.Equal(t, reasonTrustBundle, logs.failureFor(t, name)["reason"])
	}
	assert.Empty(t, logs.level(t, msgInjectionDeferred), "an SVID without a bundle is a failure, not the #740 wait")
	assert.Positive(t, injectionFailures(t, metrics, "validating", reasonTrustBundle))
	assert.Positive(t, injectionFailures(t, metrics, "mutating", reasonTrustBundle))

	bundle, err := ca.Bundle(td).Marshal()
	require.NoError(t, err)
	identity.Arrive(ca.SVID(t, testSpiffeID), ca.Bundle(td))
	require.Eventually(t, func() bool {
		return allEqual(validatingBundles(t, c), bundle) && bytes.Equal(mutatingBundleOf(t, c), bundle)
	}, eventually, tick, "logs:\n%s", logs.String())
}

// TestCABundleInjectorSteadyStateIsQuiet: once both webhooks carry the bundle,
// the injector neither writes nor retries until the next rotation.
func TestCABundleInjectorSteadyStateIsQuiet(t *testing.T) {
	var reads, writes atomic.Int64
	c := newWebhookPair(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes.Add(1)
			return c.Update(ctx, obj, opts...)
		},
	})

	log, _ := testLogger()
	identity, _ := newRotatingIdentity(t)
	injector := &CABundleInjector{
		Reader: c, Writer: c, Source: identity, Log: log,
		WebhookConfigName:         testWebhookConfigName,
		MutatingWebhookConfigName: testMutatingWebhookConfigName,
		RetryMin:                  testRetryMin,
		RetryMax:                  testRetryMax,
	}
	startInjector(t.Context(), t, injector)

	require.Eventually(t, func() bool { return writes.Load() == 2 }, eventually, tick)
	time.Sleep(settle)
	assert.Equal(t, int64(2), reads.Load(), "a successful pass must not be retried")
	assert.Equal(t, int64(2), writes.Load(), "one write per configuration")
}

// TestCABundleInjectorBackoff pins the retry schedule: doubling from the floor
// to the ceiling, with the documented defaults when nothing is configured.
func TestCABundleInjectorBackoff(t *testing.T) {
	injector := &CABundleInjector{RetryMin: 10 * time.Millisecond, RetryMax: 35 * time.Millisecond}
	var got []time.Duration
	delay := time.Duration(0)
	for range 5 {
		delay = injector.nextDelay(delay)
		got = append(got, delay)
	}
	assert.Equal(t, []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 35 * time.Millisecond, 35 * time.Millisecond, 35 * time.Millisecond,
	}, got)

	defaults := &CABundleInjector{}
	assert.Equal(t, DefaultCABundleRetryMin, defaults.nextDelay(0))
	assert.Equal(t, DefaultCABundleRetryMax, defaults.nextDelay(DefaultCABundleRetryMax))
	assert.True(t, defaults.NeedLeaderElection(), "one writer cluster-wide")
}
