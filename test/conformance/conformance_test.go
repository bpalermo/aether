//go:build conformance

// Drop-in runner for the upstream Kubernetes Gateway API conformance suite
// (sigs.k8s.io/gateway-api/conformance @ v1.5.1) against a live aether cluster.
//
// IMPORTANT — this file is NOT compiled as part of the aether Go module. The
// gateway-api *conformance* package is a SEPARATE Go module whose go.mod carries a
// `replace sigs.k8s.io/gateway-api => ../`, so it is only buildable from *inside* a
// gateway-api source checkout — it cannot be consumed as an ordinary dependency
// (`go get sigs.k8s.io/gateway-api/conformance` fails to resolve the apis/* imports).
// That is exactly why the prior runner lived in a /tmp gateway-api checkout.
//
// So this file is COPIED into a checked-out gateway-api@v1.5.1 tree at CI time
// (alongside the suite's own conformance_test.go) and run there; the committed
// `mesh/manifests.yaml` overlay is copied next to it. The `//go:build conformance`
// tag keeps it out of `go test ./...` / `bazel test //...` in this repo. See
// .github/workflows/conformance.yaml and docs/proposals/024_conformance-ci.md.
//
//   - TestAetherGatewayHTTP: the north-south edge profile. Fully conformant on talos
//     (43/43, rev21). Runs and GATES in CI (kind, no SPIRE, cleartext backends).
//   - TestAetherMeshHTTP: the east-west GAMMA profile. NOT conformant yet (blocked on
//     proposal 022). Uses the committed mesh overlay (mesh/manifests.yaml) which adds
//     the aether.io/managed namespace label, per-version ServiceAccounts, and the
//     capture.aether.io/redirect-all annotation. Kept reproducible; not yet run in CI.
//
// Both tests require a live cluster reachable via the ambient kubeconfig and are
// SKIPPED unless AETHER_CONFORMANCE=1.
package conformance_test

import (
	"context"
	"embed"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/gateway-api/apis/v1alpha3"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	xv1alpha1 "sigs.k8s.io/gateway-api/apisx/v1alpha1"
	gwconformance "sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/tests"
	conformanceconfig "sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
	"sigs.k8s.io/yaml"
)

// meshManifests is aether's overlay of the suite's base mesh manifests: the
// aether.io/managed=true namespace label, per-version ServiceAccounts for the
// echo Deployments (aether identity = ServiceAccount), and the
// capture.aether.io/redirect-all=true pod annotation (so real Service ports are
// captured). Vanilla upstream manifests would not exercise aether's mesh path.
//
//go:embed mesh/manifests.yaml
var meshManifests embed.FS

const (
	aetherGatewayClassName = "aether"
	aetherImplVersion      = "0.53.0"
)

func aetherClients(t *testing.T) (client.Client, *clientset.Clientset, client.Options, *rest.Config) {
	t.Helper()
	cfg, err := config.GetConfig()
	require.NoError(t, err, "error loading Kubernetes config")
	copts := client.Options{}
	c, err := client.New(cfg, copts)
	require.NoError(t, err, "error initializing Kubernetes client")
	cs, err := clientset.NewForConfig(cfg)
	require.NoError(t, err, "error initializing clientset")

	require.NoError(t, v1alpha3.Install(c.Scheme()))
	require.NoError(t, v1alpha2.Install(c.Scheme()))
	require.NoError(t, v1beta1.Install(c.Scheme()))
	require.NoError(t, xv1alpha1.Install(c.Scheme()))
	require.NoError(t, gatewayv1.Install(c.Scheme()))
	require.NoError(t, apiextensionsv1.AddToScheme(c.Scheme()))
	return c, cs, copts, cfg
}

// logGatewayFeatures prints the features the GatewayClass advertises. When
// SupportedFeatures is left empty the suite re-infers exactly this set from
// GatewayClass.status.supportedFeatures; logging it makes the run self-documenting.
func logGatewayFeatures(t *testing.T, c client.Client) {
	t.Helper()
	var gc gatewayv1.GatewayClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: aetherGatewayClassName}, &gc))
	var fns []features.FeatureName
	for _, sf := range gc.Status.SupportedFeatures {
		fns = append(fns, features.FeatureName(string(sf.Name)))
	}
	t.Logf("Supported features for GatewayClass %s: %v", aetherGatewayClassName, fns)
}

// aetherTimeouts mirrors the talos baseline runner: a long GatewayMustHaveAddress
// budget (LoadBalancer/MetalLB or cloud-provider-kind convergence is slow) and a
// generous consistency window for demand-scoped xDS propagation.
func aetherTimeouts() conformanceconfig.TimeoutConfig {
	tc := conformanceconfig.DefaultTimeoutConfig()
	tc.GatewayMustHaveAddress = 180 * time.Second
	tc.MaxTimeToConsistency = 60 * time.Second
	tc.RequestTimeout = 10 * time.Second
	return tc
}

func aetherImpl() confv1.Implementation {
	return confv1.Implementation{
		Organization: "aether",
		Project:      "aether",
		URL:          "https://github.com/bpalermo/aether",
		Version:      aetherImplVersion,
		Contact:      []string{"bpalermo@pm.me"},
	}
}

// cleanup reports whether the suite should tear down its base resources. Set
// AETHER_NOCLEANUP=1 to leave them in place for post-mortem debugging.
func cleanup() bool { return os.Getenv("AETHER_NOCLEANUP") == "" }

// reportPath is where the YAML ConformanceReport is written. CI uploads it as an
// artifact. Override with AETHER_CONFORMANCE_REPORT.
func reportPath(def string) string {
	if v := os.Getenv("AETHER_CONFORMANCE_REPORT"); v != "" {
		return v
	}
	return def
}

func writeReport(t *testing.T, cSuite *suite.ConformanceTestSuite, path string) {
	t.Helper()
	report, err := cSuite.Report()
	require.NoError(t, err)
	raw, err := yaml.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	t.Logf("Conformance report written to %s:\n%s", path, string(raw))
}

func skipUnlessEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("AETHER_CONFORMANCE") == "" {
		t.Skip("set AETHER_CONFORMANCE=1 (and point KUBECONFIG at a live aether cluster) to run")
	}
}

// TestAetherGatewayHTTP runs the GATEWAY-HTTP (north-south edge) conformance
// profile. Features are inferred from GatewayClass.status.supportedFeatures
// (SupportedFeatures left nil + EnableAllSupportedFeatures=false). This is the
// profile aether is fully conformant on (43/43 on talos, rev21) and the one CI gates.
func TestAetherGatewayHTTP(t *testing.T) {
	skipUnlessEnabled(t)
	c, cs, copts, cfg := aetherClients(t)
	logGatewayFeatures(t, c)

	opts := suite.ConformanceOptions{
		Client:                     c,
		ClientOptions:              copts,
		Clientset:                  cs,
		RestConfig:                 cfg,
		GatewayClassName:           aetherGatewayClassName,
		AllowCRDsMismatch:          true,
		CleanupBaseResources:       cleanup(),
		EnableAllSupportedFeatures: false,
		SupportedFeatures:          nil, // inferred from GatewayClass.status
		ExemptFeatures:             nil,
		// NewConformanceTestSuite (unlike the RunConformance helper) does not default
		// ManifestFS, so set the embedded base manifests explicitly — otherwise the
		// suite applies no base resources and the conformance namespaces (e.g.
		// gateway-conformance-web-backend) are never created.
		ManifestFS:          []fs.FS{&gwconformance.Manifests},
		TimeoutConfig:       aetherTimeouts(),
		ConformanceProfiles: sets.New(suite.GatewayHTTPConformanceProfileName),
		Implementation:             aetherImpl(),
		ReportOutputPath:           reportPath("gateway-http-report.yaml"),
	}

	cSuite, err := suite.NewConformanceTestSuite(opts)
	require.NoError(t, err)
	cSuite.Setup(t, tests.ConformanceTests)
	require.NoError(t, cSuite.Run(t, tests.ConformanceTests))
	writeReport(t, cSuite, opts.ReportOutputPath)
}

// TestAetherMeshHTTP runs the MESH-HTTP (east-west GAMMA) conformance profile
// against the committed mesh overlay. This is NOT conformant yet (blocked on
// proposal 022 — arbitrary-service interception) and is therefore not run by the
// CI workflow; it is kept here so a MESH run is reproducible the day 022 unblocks it.
func TestAetherMeshHTTP(t *testing.T) {
	skipUnlessEnabled(t)
	if os.Getenv("AETHER_CONFORMANCE_MESH") == "" {
		t.Skip("MESH-HTTP is not conformant yet (proposal 022); set AETHER_CONFORMANCE_MESH=1 to run anyway")
	}
	c, cs, copts, cfg := aetherClients(t)

	meshFeats := sets.New(
		features.SupportMesh,
		features.SupportHTTPRoute,
		features.SupportHTTPRouteResponseHeaderModification,
		features.SupportHTTPRouteMethodMatching,
		features.SupportHTTPRouteRequestTimeout,
	)

	opts := suite.ConformanceOptions{
		Client:                     c,
		ClientOptions:              copts,
		Clientset:                  cs,
		RestConfig:                 cfg,
		GatewayClassName:           aetherGatewayClassName,
		MeshName:                   "aether",
		AllowCRDsMismatch:          true,
		CleanupBaseResources:       cleanup(),
		EnableAllSupportedFeatures: false,
		SupportedFeatures:          meshFeats,
		// Use aether's mesh overlay instead of the suite's base mesh manifests.
		ManifestFS:          []fs.FS{meshManifests},
		TimeoutConfig:       aetherTimeouts(),
		ConformanceProfiles: sets.New(suite.MeshHTTPConformanceProfileName),
		Implementation:      aetherImpl(),
		ReportOutputPath:    reportPath("mesh-http-report.yaml"),
	}

	cSuite, err := suite.NewConformanceTestSuite(opts)
	require.NoError(t, err)
	cSuite.Setup(t, tests.ConformanceTests)
	require.NoError(t, cSuite.Run(t, tests.ConformanceTests))
	writeReport(t, cSuite, opts.ReportOutputPath)
}
