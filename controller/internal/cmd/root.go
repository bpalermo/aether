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
	"strconv"
	"strings"

	crdv1 "aethermesh.dev/common/apis/config/v1"
	"aethermesh.dev/common/manager"
	"aethermesh.dev/common/spire"
	"aethermesh.dev/controller/internal/edgeconfig"
	"aethermesh.dev/controller/internal/endpointpolicy"
	"aethermesh.dev/controller/internal/gatewayapi"
	"aethermesh.dev/controller/internal/httpfilter"
	"aethermesh.dev/controller/internal/meshconfig"
	"aethermesh.dev/controller/internal/nodetaint"
	"aethermesh.dev/controller/internal/podmutate"
	cwebhook "aethermesh.dev/controller/internal/webhook"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const name = "aether-controller"

// spireComponent namespaces this binary's SPIRE identity-wait metrics
// (aether.controller.spire.wait_seconds and friends).
const spireComponent = "controller"

// readyzAdder is the one thing wireSpireReadiness needs from the manager. A
// narrow interface rather than ctrl.Manager keeps the readiness table testable
// without an apiserver.
type readyzAdder interface {
	AddReadyzCheck(name string, check healthz.Checker) error
}

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
		l, logShutdown = manager.SetupManagerLogging(cmd.Context(), cfg.Config, name, Version)
		return nil
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
	rootCmd.Flags().DurationVar(&cfg.SpireWaitWarnAfter, "spire-wait-warn-after", cfg.SpireWaitWarnAfter, "How long to wait for this workload's first SVID before escalating the waiting log line to WARN (issue #740)")
	rootCmd.Flags().StringVar(&cfg.WebhookConfigName, "webhook-config-name", cfg.WebhookConfigName, "ValidatingWebhookConfiguration to patch with the SPIRE caBundle (SPIRE mode)")
	rootCmd.Flags().StringVar(&cfg.MutatingWebhookConfigName, "mutating-webhook-config-name", cfg.MutatingWebhookConfigName, "MutatingWebhookConfiguration (pod ndots) to patch with the SPIRE caBundle (SPIRE mode); empty disables")
	rootCmd.Flags().StringVar(&cfg.MeshDomain, "mesh-domain", cfg.MeshDomain, "DNS-style domain mesh authorities live under; the pod-mutating webhook derives the injected dnsConfig ndots from its label count (2 for aether.internal)")
}

// podNDots derives the dnsConfig ndots the pod-mutating webhook injects from
// the mesh domain's label count, so it can never drift from the domain it
// describes (the retired --pod-ndots flag could).
func podNDots(meshDomain string) string {
	return strconv.Itoa(strings.Count(meshDomain, ".") + 1)
}

func runController(ctx context.Context) (retErr error) {
	l.InfoContext(
		ctx, "starting aether controller",
		"metricsEnabled", cfg.MetricsEnabled,
		"otelEnabled", cfg.OTelEnabled,
		"leaderElection", cfg.LeaderElection,
		"spireEnabled", cfg.SpireEnabled,
	)

	defer deferControllerLogShutdown(ctx)

	spireSource, bootstrapOpts, err := buildControllerBootstrapOpts(ctx)
	if err != nil {
		return err
	}
	if spireSource != nil {
		defer func() { retErr = errors.Join(retErr, spireSource.Close()) }()
	}

	result, err := manager.Bootstrap(ctx, cfg.Config, name, Version, l, bootstrapOpts...)
	if err != nil {
		return err
	}
	defer deferControllerTelemetryShutdown(ctx, result.Shutdown)

	m := result.Manager

	if err = wireSpireReadiness(m, spireSource); err != nil {
		return err
	}

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

	if err = wireNodeTaintGuard(m, fallbackNamespace); err != nil {
		return err
	}

	// All validation is served on one /validate endpoint, dispatched by Kind. The
	// HTTPRoute validator uses the API reader for a cluster-wide hostname-conflict
	// list (uncached, correct regardless of the manager cache scope).
	cwebhook.NewHandler(l, map[string]admission.Handler{
		crdv1.MeshConfigKind:     &meshconfig.Validator{Log: l},
		crdv1.EdgeConfigKind:     &edgeconfig.Validator{Log: l},
		crdv1.HTTPFilterKind:     &httpfilter.Validator{Reader: m.GetAPIReader(), Log: l},
		crdv1.EndpointPolicyKind: &endpointpolicy.Validator{Log: l},
		gatewayapi.HTTPRouteKind: &gatewayapi.Validator{Reader: m.GetAPIReader(), Log: l},
	}).SetupWithManager(m)

	// Pod-ndots mutating webhook (musl mesh-FQDN resolution; opt-in via the chart's
	// MutatingWebhookConfiguration, scoped to managed pods, failurePolicy=Ignore).
	// Served on /mutate (mirrors the shared /validate endpoint); inert unless the
	// apiserver routes pods here.
	m.GetWebhookServer().Register("/mutate", &admission.Webhook{Handler: podmutate.NewMutator(podNDots(cfg.MeshDomain), l)})

	if err = wireCABundleInjector(m, spireSource); err != nil {
		return err
	}

	l.InfoContext(
		ctx, "MeshConfig controller configured",
		"configMapName", cfg.MeshConfigMapName,
		"fallbackNamespace", fallbackNamespace,
		"webhookPath", cwebhook.Path,
	)
	return m.Start(ctx)
}

// buildControllerBootstrapOpts builds the manager scheme and SPIRE webhook options.
// When SPIRE is enabled it starts acquiring the Workload API source in the
// background and configures the webhook to serve with the X.509 SVID; otherwise
// the default Helm-provisioned cert is used. The caller is responsible for closing
// the returned spireSource.
//
// This is the EARLIEST SPIRE call site of the four (issue #740): it runs before
// manager.Bootstrap, because the webhook server is built from the options it
// returns. It used to be a blocking, fatal 25s wait for the first SVID, which made
// a cold boot — SPIRE server and controller starting together — exit the process
// with "failed to open SPIRE Workload API source: context deadline exceeded" and
// crash-loop until SPIRE won the race.
//
// Nothing here needs the SVID at construction time: WebhookServerCert installs
// tls.Config.GetCertificate, which is consulted per HANDSHAKE, so a source that is
// still waiting fails individual TLS handshakes instead of the process. Both
// webhook configurations are failurePolicy: Ignore, so the apiserver fails OPEN
// for the duration — admission proceeds unvalidated and un-mutated, which is what
// it already did while the container was crash-looping, only now the controller is
// alive to start serving the moment the SVID lands.
func buildControllerBootstrapOpts(ctx context.Context) (*spire.WaitingSource, []func(*ctrl.Options), error) {
	// Manager scheme = client-go built-ins + the typed MeshConfig CRD, so the
	// reconciler and webhook work against the typed object (no unstructured).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("register client-go scheme: %w", err)
	}
	if err := crdv1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("register MeshConfig scheme: %w", err)
	}
	// Gateway API types: the HTTPRoute validating webhook lists HTTPRoutes for the
	// hostname-conflict check (proposal 018).
	if err := gatewayv1.Install(scheme); err != nil {
		return nil, nil, fmt.Errorf("register gateway.networking.k8s.io scheme: %w", err)
	}

	bootstrapOpts := []func(*ctrl.Options){func(o *ctrl.Options) { o.Scheme = scheme }}

	// When SPIRE is enabled, serve the validating webhook with a SPIRE X.509 SVID
	// (one-way TLS; the apiserver verifies against the SPIRE bundle injected as the
	// caBundle). Otherwise controller-runtime serves from the Helm-provisioned cert
	// in the default CertDir.
	if !cfg.SpireEnabled {
		return nil, bootstrapOpts, nil
	}
	src := spire.NewWaitingSource(cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l, spire.WithComponent(spireComponent))
	// There is no manager to register the runnable with yet — the manager is built
	// FROM these options — so the acquisition runs on the command context, which
	// SIGTERM cancels exactly as it does the manager's.
	go func() { _ = src.Start(ctx) }()
	bootstrapOpts = append(bootstrapOpts, func(o *ctrl.Options) {
		o.WebhookServer = webhook.NewServer(webhook.Options{
			TLSOpts: []func(*tls.Config){spire.WebhookServerCert(src)},
		})
	})
	l.InfoContext(ctx, "webhook will serve with the SPIRE SVID; startup does not wait for SPIRE",
		"socket", cfg.SpireWorkloadSocketPath, "warnAfter", cfg.SpireWaitWarnAfter)
	return src, bootstrapOpts, nil
}

// wireSpireReadiness registers the identity half of the controller's readiness
// (issue #740). It is load-bearing here in a way it is not for a DaemonSet: the
// controller is behind a Service, so NotReady removes this replica from the
// webhook Service's endpoints — the apiserver stops routing admission to a replica
// that cannot complete a TLS handshake and (failurePolicy: Ignore) fails open
// immediately instead of waiting out its 10s webhook timeout on every request.
//
// There is NO dwell (commonspire.ServiceNotReadyDwell, #740 PR 4). The agent's 2m
// dwell is not about tolerating a cold boot for its own sake — it exists because
// NotReady on the agent DaemonSet is what arms the node taint, and a taint is a
// fleet-level action a 40s SPIRE hiccup must not trigger. Leaving a Service's
// endpoints has no such blast radius: it is the exact, local, immediately
// reversible handling of a replica that cannot complete a TLS handshake.
func wireSpireReadiness(m readyzAdder, src *spire.WaitingSource) error {
	if src == nil {
		return nil // --spire-enabled=false: no identity to wait for, no check
	}
	if err := m.AddReadyzCheck(spire.ReadyCheckName, spire.ReadyChecker(src, spire.ServiceNotReadyDwell)); err != nil {
		return fmt.Errorf("failed to set up the SPIRE identity ready check: %w", err)
	}
	return nil
}

// wireNodeTaintGuard registers the leader-elected node-taint guard, which
// re-arms the aether startup taint on a node whose agent pod is missing/not-Ready
// past a grace period (issue #569: reboots don't re-apply register-with-taints,
// and the old one-shot remover never re-armed). The agent DaemonSet runs in the
// controller's own (fallback) namespace, so the guard selects agent pods there.
// The guard never removes the taint — the per-node agent owns removal.
func wireNodeTaintGuard(m ctrl.Manager, agentNamespace string) error {
	guard := &nodetaint.Guard{
		Client:         m.GetClient(),
		AgentNamespace: agentNamespace,
		Log:            l,
	}
	if err := guard.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to set up node-taint guard: %w", err)
	}
	return nil
}

// wireCABundleInjector registers the CA bundle injector when SPIRE is enabled.
// In SPIRE mode the webhook presents an SVID, so the apiserver must trust the
// SPIRE CA: keep the ValidatingWebhookConfiguration caBundle in sync with the
// rotating trust bundle.
//
// The source may not hold an SVID yet (issue #740). The injector handles that by
// deferring: it logs the first attempt at INFO and injects on the Updated() wake
// that WaitingSource fires the moment the SVID lands.
func wireCABundleInjector(m ctrl.Manager, spireSource *spire.WaitingSource) error {
	if !cfg.SpireEnabled || spireSource == nil {
		return nil
	}
	injector := &meshconfig.CABundleInjector{
		Client:                    m.GetClient(),
		Source:                    spireSource,
		WebhookConfigName:         cfg.WebhookConfigName,
		MutatingWebhookConfigName: cfg.MutatingWebhookConfigName,
		Log:                       l,
	}
	if err := m.Add(injector); err != nil {
		return fmt.Errorf("failed to add caBundle injector: %w", err)
	}
	return nil
}

// deferControllerLogShutdown flushes and stops the OTLP log exporter. No-op when
// logShutdown is nil (OTLP logging is disabled).
func deferControllerLogShutdown(ctx context.Context) {
	if logShutdown == nil {
		return
	}
	if err := logShutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to flush OTel logs", "error", err)
	}
}

// deferControllerTelemetryShutdown runs the telemetry shutdown returned by
// manager.Bootstrap. No-op when shutdown is nil.
func deferControllerTelemetryShutdown(ctx context.Context, shutdown func(context.Context) error) {
	if shutdown == nil {
		return
	}
	if err := shutdown(ctx); err != nil {
		l.ErrorContext(ctx, "failed to shutdown telemetry", "error", err)
	}
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
