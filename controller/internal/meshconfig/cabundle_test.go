package meshconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/common/spire"
	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testWebhookConfigName = "aether-validating-webhook"
	testSpiffeID          = "spiffe://" + spiretest.TrustDomain + "/ns/aether-system/sa/aether-controller"
)

// lockedBuffer is a concurrency-safe log sink: the injector logs from its own
// goroutine while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records returns the JSON log records emitted so far.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		out = append(out, rec)
	}
	return out
}

// level returns the level a message was logged at, or "" if it was not logged.
func (b *lockedBuffer) level(t *testing.T, msg string) string {
	t.Helper()

	for _, rec := range b.records(t) {
		if rec["msg"] == msg {
			level, _ := rec["level"].(string)
			return level
		}
	}
	return ""
}

// newWebhookClient returns a fake client holding an un-injected
// ValidatingWebhookConfiguration.
func newWebhookClient(t *testing.T) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testWebhookConfigName},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name:                    "meshconfig.aether.io",
			SideEffects:             ptr(admissionregistrationv1.SideEffectClassNone),
			AdmissionReviewVersions: []string{"v1"},
		}},
	}).Build()
}

func ptr[T any](v T) *T { return &v }

// caBundle reads the caBundle currently on the webhook configuration.
func caBundle(t *testing.T, c client.Client) []byte {
	t.Helper()

	var vwc admissionregistrationv1.ValidatingWebhookConfiguration
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: testWebhookConfigName}, &vwc))
	return vwc.Webhooks[0].ClientConfig.CABundle
}

// TestCABundleInjectorDefersUntilTheSVIDArrives is the controller half of #740:
// the injector starts before SPIRE has issued this workload's first SVID, must
// say so at INFO rather than ERROR (there is nothing an operator can do, and the
// webhook fails open meanwhile), and must inject the bundle on the Updated() wake
// that WaitingSource fires when the SVID lands — without any retry logic of its
// own.
func TestCABundleInjectorDefersUntilTheSVIDArrives(t *testing.T) {
	wlapi, sock := spiretest.Start(t, testSpiffeID)
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	src := spire.NewWaitingSource(sock, time.Hour, log)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = src.Start(ctx) }()
	t.Cleanup(func() { _ = src.Close() })

	c := newWebhookClient(t)
	injector := &CABundleInjector{
		Client:            c,
		Source:            src,
		WebhookConfigName: testWebhookConfigName,
		Log:               log,
	}
	done := make(chan error, 1)
	go func() { done <- injector.Start(ctx) }()

	// Before the SVID: the deferral is announced at INFO, nothing is injected, and
	// the ERROR the pre-#740 code would have logged is absent.
	require.Eventually(t, func() bool {
		return logs.level(t, "webhook caBundle injection deferred until this workload has an SVID") == "INFO"
	}, 30*time.Second, 10*time.Millisecond, "logs:\n%s", logs.String())
	assert.Empty(t, logs.level(t, "initial webhook caBundle injection failed"),
		"a missing SVID is a wait, not an error to report")
	assert.Empty(t, caBundle(t, c), "nothing can be injected before the trust bundle exists")

	// The SVID lands: WaitingSource fires Updated(), and the injector — already
	// parked on that channel — writes the bundle.
	wlapi.StartServing()

	require.Eventually(t, func() bool {
		return len(caBundle(t, c)) > 0
	}, 30*time.Second, 20*time.Millisecond, "the caBundle must be injected once the SVID lands; logs:\n%s", logs.String())
	assert.Contains(t, string(caBundle(t, c)), "BEGIN CERTIFICATE", "the caBundle must be PEM")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a SPIRE outage must never fail the injector")
	case <-time.After(30 * time.Second):
		t.Fatal("the injector did not return after cancellation")
	}
}
