package meshconfig

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/bpalermo/aether/common/spire"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CABundleInjector keeps the validating webhook's caBundle in sync with the
// SPIRE trust bundle. The webhook is served with a SPIRE X.509 SVID, so the
// kube-apiserver must trust the SPIRE CA — this runnable writes the current
// bundle into the ValidatingWebhookConfiguration on startup and on every SVID
// rotation. It runs on the leader only (a cluster-wide single writer).
type CABundleInjector struct {
	Client            client.Client
	Source            *spire.Source
	WebhookConfigName string
	Log               *slog.Logger
}

// NeedLeaderElection keeps caBundle writes to a single replica.
func (i *CABundleInjector) NeedLeaderElection() bool { return true }

// Start injects the bundle once, then re-injects whenever the SPIRE source
// reports a rotation, until the context is cancelled.
func (i *CABundleInjector) Start(ctx context.Context) error {
	if err := i.inject(ctx); err != nil {
		// Don't fail startup: failurePolicy=Ignore means an un-injected webhook
		// fails open, and the next rotation tick retries.
		i.Log.ErrorContext(ctx, "initial webhook caBundle injection failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-i.Source.Updated():
			if err := i.inject(ctx); err != nil {
				i.Log.ErrorContext(ctx, "webhook caBundle injection failed", "error", err)
			}
		}
	}
}

// inject sets the current SPIRE trust bundle as the caBundle on every webhook in
// the ValidatingWebhookConfiguration via read-modify-write (Update), NOT
// Server-Side Apply: the `webhooks` list is atomic for SSA, so a partial apply
// would replace the whole entry and drop Helm-owned required fields
// (sideEffects, admissionReviewVersions). This is the standard caBundle-injection
// pattern (cf. cert-manager's cainjector).
func (i *CABundleInjector) inject(ctx context.Context) error {
	bundle, err := spire.TrustBundlePEM(i.Source)
	if err != nil {
		return err
	}

	var vwc admissionregistrationv1.ValidatingWebhookConfiguration
	if err := i.Client.Get(ctx, types.NamespacedName{Name: i.WebhookConfigName}, &vwc); err != nil {
		return fmt.Errorf("get ValidatingWebhookConfiguration %q: %w", i.WebhookConfigName, err)
	}

	changed := false
	for idx := range vwc.Webhooks {
		if !bytes.Equal(vwc.Webhooks[idx].ClientConfig.CABundle, bundle) {
			vwc.Webhooks[idx].ClientConfig.CABundle = bundle
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := i.Client.Update(ctx, &vwc); err != nil {
		return fmt.Errorf("update webhook caBundle: %w", err)
	}
	i.Log.InfoContext(ctx, "injected SPIRE trust bundle into webhook caBundle", "webhookConfig", i.WebhookConfigName)
	return nil
}
