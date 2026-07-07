// Package cmd provides the command-line interface and runtime for the
// aether-controller.
//
// The aether-controller is the in-cluster owner of mesh-wide configuration. It
// runs a controller-runtime manager (with leader election) that:
//   - serves a validating admission webhook for the MeshConfig CRD, rejecting
//     specs that fail protovalidate, and
//   - reconciles the singleton MeshConfig CR into a ConfigMap that the agent and
//     registrar mount and load.
//
// Keeping this out of the registrar means the registrar (and agent) consume mesh
// config uniformly from the mounted ConfigMap, and the singleton controller owns
// leader election and webhook serving on its own resource budget.
package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"

	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/manager"
	"github.com/bpalermo/aether/common/spire"
	"github.com/bpalermo/aether/controller/internal/edgeconfig"
	"github.com/bpalermo/aether/controller/internal/gatewayapi"
	"github.com/bpalermo/aether/controller/internal/httpfilter"
	"github.com/bpalermo/aether/controller/internal/meshconfig"
	"github.com/bpalermo/aether/controller/internal/podmutate"
	cwebhook "github.com/bpalermo/aether/controller/internal/webhook"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const name = "aether-controller"

// Version is set at build time via -ldflags (Bazel x_defs).
var Version = "dev"

var (
	cfg = NewControllerConfig()

	l           *slog.Logger
	logShutdown func(context.Context) error
)

var rootCmd = &cobra.Command{
	Use:          "controller",
	Short:        "Runs the aether mesh-config controller.",
	Long:         "Runs the aether-controller: the MeshConfig validating webhook and the reconciler that projects the MeshConfig CR into a ConfigMap.",
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) (err error) {
		l, logShutdown, err = manager.SetupManagerLogging(cmd.Context(), cfg.Config, name, Version)
		return err
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runController(cmd.Context())
	},
}

// GetCommand returns the root cobra command for the controller.
func GetCommand() *cobra.Command {
	return rootCmd
}

func init() {
	manager.RegisterFlags(rootCmd, &cfg.Config)
	rootCmd.Flags().StringVar(&cfg.MeshConfigMapName, "mesh-config-configmap", cfg.MeshConfigMapName, "Name of the ConfigMap the MeshConfig reconciler projects into (in each MeshConfig's own namespace)")
	rootCmd.Flags().BoolVar(&cfg.SpireEnabled, "spire-enabled", cfg.SpireEnabled, "Serve the validating webhook with a SPIRE X.509 SVID and inject the SPIRE trust bundle into the webhook caBundle (instead of a static cert)")
	rootCmd.Flags().StringVar(&cfg.SpireWorkloadSocketPath, "spire-workload-socket", cfg.SpireWorkloadSocketPath, "Path to the SPIRE Workload API UDS socket")
	rootCmd.Flags().StringVar(&cfg.WebhookConfigName, "webhook-config-name", cfg.WebhookConfigName, "ValidatingWebhookConfiguration to patch with the SPIRE caBundle (SPIRE mode)")
	rootCmd.Flags().StringVar(&cfg.MutatingWebhookConfigName, "mutating-webhook-config-name", cfg.MutatingWebhookConfigName, "MutatingWebhookConfiguration (pod ndots) to patch with the SPIRE caBundle (SPIRE mode); empty disables")
	rootCmd.Flags().StringVar(&cfg.PodNDots, "pod-ndots", cfg.PodNDots, "dnsConfig ndots the pod-mutating webhook injects into managed pods (the mesh-domain label count; 2 for aether.internal)")
}

func runController(ctx context.Context) (retErr error) {
	l.InfoContext(ctx, "starting aether controller",
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
		"leaderElection", cfg.LeaderElection,
		"spireEnabled", cfg.SpireEnabled,
	)

	if logShutdown != nil {
		defer func() {
			if shutdownErr := logShutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to flush OTel logs", "error", shutdownErr)
			}
		}()
	}

	// Manager scheme = client-go built-ins + the typed MeshConfig CRD, so the
	// reconciler and webhook work against the typed object (no unstructured).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := crdv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register MeshConfig scheme: %w", err)
	}
	// Gateway API types: the HTTPRoute validating webhook lists HTTPRoutes for the
	// hostname-conflict check (proposal 018).
	if err := gatewayv1.Install(scheme); err != nil {
		return fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}

	// When SPIRE is enabled, serve the validating webhook with a SPIRE X.509 SVID
	// (one-way TLS; the apiserver verifies against the SPIRE bundle injected as the
	// caBundle). Otherwise controller-runtime serves from the Helm-provisioned cert
	// in the default CertDir.
	var spireSource *spire.Source
	bootstrapOpts := []func(*ctrl.Options){func(o *ctrl.Options) { o.Scheme = scheme }}
	if cfg.SpireEnabled {
		src, srcErr := spire.NewSource(ctx, cfg.SpireWorkloadSocketPath)
		if srcErr != nil {
			return fmt.Errorf("failed to open SPIRE Workload API source: %w", srcErr)
		}
		spireSource = src
		defer func() { retErr = errors.Join(retErr, spireSource.Close()) }()

		bootstrapOpts = append(bootstrapOpts, func(o *ctrl.Options) {
			o.WebhookServer = webhook.NewServer(webhook.Options{
				TLSOpts: []func(*tls.Config){spire.WebhookServerCert(spireSource)},
			})
		})
		l.InfoContext(ctx, "webhook serving with SPIRE SVID", "socket", cfg.SpireWorkloadSocketPath)
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, name, Version, bootstrapOpts...)
	if err != nil {
		return err
	}
	if result.Shutdown != nil {
		defer func() {
			if shutdownErr := result.Shutdown(ctx); shutdownErr != nil {
				l.ErrorContext(ctx, "failed to shutdown telemetry", "error", shutdownErr)
			}
		}()
	}

	m := result.Manager

	// The controller's own namespace holds the canonical (fallback) MeshConfig that
	// other namespaces inherit from unless they set their own.
	fallbackNamespace := currentNamespace()

	reconciler := &meshconfig.Reconciler{
		Client:            m.GetClient(),
		ConfigMapName:     cfg.MeshConfigMapName,
		FallbackNamespace: fallbackNamespace,
		Log:               l,
	}
	if err = reconciler.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up MeshConfig reconciler: %w", err)
	}

	// All validation is served on one /validate endpoint, dispatched by Kind. The
	// HTTPRoute validator uses the API reader for a cluster-wide hostname-conflict
	// list (uncached, correct regardless of the manager cache scope).
	cwebhook.NewHandler(l, map[string]admission.Handler{
		crdv1.MeshConfigKind:     &meshconfig.Validator{Log: l},
		crdv1.EdgeConfigKind:     &edgeconfig.Validator{Log: l},
		crdv1.HTTPFilterKind:     &httpfilter.Validator{Reader: m.GetAPIReader(), Log: l},
		gatewayapi.HTTPRouteKind: &gatewayapi.Validator{Reader: m.GetAPIReader(), Log: l},
	}).SetupWithManager(m)

	// Pod-ndots mutating webhook (musl mesh-FQDN resolution; opt-in via the chart's
	// MutatingWebhookConfiguration, scoped to managed pods, failurePolicy=Ignore).
	// Served on /mutate (mirrors the shared /validate endpoint); inert unless the
	// apiserver routes pods here.
	m.GetWebhookServer().Register("/mutate", &admission.Webhook{Handler: podmutate.NewMutator(cfg.PodNDots, l)})

	// In SPIRE mode the webhook presents an SVID, so the apiserver must trust the
	// SPIRE CA: keep the ValidatingWebhookConfiguration caBundle in sync with the
	// rotating trust bundle.
	if cfg.SpireEnabled {
		injector := &meshconfig.CABundleInjector{
			Client:                    m.GetClient(),
			Source:                    spireSource,
			WebhookConfigName:         cfg.WebhookConfigName,
			MutatingWebhookConfigName: cfg.MutatingWebhookConfigName,
			Log:                       l,
		}
		if err = m.Add(injector); err != nil {
			return fmt.Errorf("failed to add caBundle injector: %w", err)
		}
	}

	l.InfoContext(ctx, "MeshConfig controller configured",
		"configMapName", cfg.MeshConfigMapName,
		"fallbackNamespace", fallbackNamespace,
		"webhookPath", cwebhook.Path,
	)
	return m.Start(ctx)
}

// currentNamespace resolves the namespace the controller runs in, used as the
// default target for the projected ConfigMap. It reads POD_NAMESPACE (set via
// the downward API by the chart) and falls back to the service-account namespace
// file.
func currentNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(data)
	}
	return "default"
}
