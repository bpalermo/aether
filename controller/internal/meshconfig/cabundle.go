package meshconfig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bpalermo/aether/common/spire"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (i *CABundleInjector) inject(ctx context.Context) error {
	bundle, err := spire.TrustBundlePEM(i.Source)
	if err != nil {
		return err
	}

	// Read the existing webhook names so the Server-Side Apply targets the real
	// list entries (webhooks is a list-map keyed by name) rather than creating a
	// phantom one. The controller's field manager then owns ONLY each webhook's
	// caBundle; Helm keeps ownership of the rest of the config.
	var existing admissionregistrationv1.ValidatingWebhookConfiguration
	if err := i.Client.Get(ctx, types.NamespacedName{Name: i.WebhookConfigName}, &existing); err != nil {
		return fmt.Errorf("get ValidatingWebhookConfiguration %q: %w", i.WebhookConfigName, err)
	}

	apply := &admissionregistrationv1.ValidatingWebhookConfiguration{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{Name: i.WebhookConfigName},
	}
	for _, w := range existing.Webhooks {
		apply.Webhooks = append(apply.Webhooks, admissionregistrationv1.ValidatingWebhook{
			Name:         w.Name,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: bundle},
		})
	}
	if err := i.Client.Patch(ctx, apply, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply webhook caBundle: %w", err)
	}
	i.Log.InfoContext(ctx, "injected SPIRE trust bundle into webhook caBundle", "webhookConfig", i.WebhookConfigName)
	return nil
}
