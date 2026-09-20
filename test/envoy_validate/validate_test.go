// Package envoy_validate runs "envoy --mode validate" over aether-generated
// bootstrap configs to catch "Envoy would NACK this" regressions before they
// reach production.
//
// The Envoy binary is provided as a Bazel data dependency: //bazel/proxy_pin
// extracts /usr/local/bin/envoy from the aether-proxy image at the digest
// //charts/aether:values.yaml pins, so this gate runs the exact binary the mesh
// deploys (custom build, most upstream extensions compiled out — see #709).
// To run the test:
//
//	bazel test //test/envoy_validate:envoy_validate_test
//	bazel test //test/envoy_validate:envoy_validate_test --test_output=all
//
// What the test catches (examples from production incidents):
//   - ORIGINAL_DST cluster with ROUND_ROBIN lb_policy      → CDS NACK, exit 1
//   - Listener with no address field                        → LDS NACK, exit 1
//   - Malformed / missing SAN in TLS validation context     → config rejected
//   - Unknown TypedConfig @type URL (non-stripped filter)   → config rejected
package envoy_validate

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// envoyBinary returns the path to the Envoy binary from the Bazel runfiles tree.
//
// In Bazel 9 bzlmod, repos created by a module extension have a canonical name
// like "+pinned_proxy+<repo-name>" rather than just "<repo-name>".  The repo
// mapping file (runfiles/_repo_mapping or .runfiles.repo_mapping) translates the
// user-visible name to the canonical name.  This function reads that mapping so
// the lookup is robust across Bazel versions.
func envoyBinary(t *testing.T) string {
	t.Helper()

	// Determine the architecture-specific user-visible repo name.
	var repoName string
	switch runtime.GOARCH {
	case "amd64":
		repoName = "pinned_envoy_linux_amd64"
	case "arm64":
		repoName = "pinned_envoy_linux_arm64"
	default:
		t.Skipf("envoy binary not available for GOARCH=%s", runtime.GOARCH)
	}

	runfiles := os.Getenv("RUNFILES_DIR")
	if runfiles == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		runfiles = exe + ".runfiles"
	}

	// Resolve the canonical repo name from the repo mapping file.
	// Format: "<from-canonical>,<apparent>,<to-canonical>"
	// We want lines starting with "," (main repo context) mapping our user name.
	canonical := canonicalRepo(t, runfiles, repoName)

	p := filepath.Join(runfiles, canonical, "envoy")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("envoy binary not found at %s: %v\n(RUNFILES_DIR=%s)", p, err, runfiles)
	}
	return p
}

// canonicalRepo resolves a user-visible repository name to its canonical bzlmod
// name by reading the _repo_mapping file in the runfiles directory.
func canonicalRepo(t *testing.T, runfiles, apparent string) string {
	t.Helper()

	// Try the direct path first (for when the file is in the runfiles root).
	for _, name := range []string{"_repo_mapping", filepath.Join("_main", "_repo_mapping")} {
		mappingPath := filepath.Join(runfiles, name)
		canonical, ok := lookupMapping(t, mappingPath, apparent)
		if ok {
			return canonical
		}
	}

	// Fall back to the direct name (pre-bzlmod or old Bazel versions).
	t.Logf("no repo mapping found for %q; falling back to direct name", apparent)
	return apparent
}

// lookupMapping reads a Bazel repo mapping file and returns the canonical name
// for the given apparent name in the main workspace context ("" or "_main").
func lookupMapping(t *testing.T, path, apparent string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		from, app, to := parts[0], parts[1], parts[2]
		// Lines with empty from-canonical are from the main workspace context.
		if from == "" && app == apparent {
			return to, true
		}
	}
	return "", false
}

// TestEnvoyValidate generates the representative aether bootstrap configs
// (node mTLS, node cleartext/SPIRE-off, transparent capture, capture route-target,
// edge) and validates each one with "envoy --mode validate".
//
// Envoy exits 0 when the config is structurally valid; exits 1 on any error.
func TestEnvoyValidate(t *testing.T) {
	envoy := envoyBinary(t)

	outDir := t.TempDir()

	builders := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"node_bootstrap.json", NodeBootstrapJSON},
		{"node_cleartext_bootstrap.json", NodeCleartextBootstrapJSON},
		{"node_uds_bootstrap.json", NodeUDSBootstrapJSON},
		{"capture_bootstrap.json", CaptureBootstrapJSON},
		{"capture_route_target_bootstrap.json", CaptureRouteTargetBootstrapJSON},
		{"outbound_zero_vhost_route_bootstrap.json", OutboundZeroVhostRouteBootstrapJSON},
		{"edge_bootstrap.json", EdgeBootstrapJSON},
	}

	// Write all bootstrap files.
	for _, b := range builders {
		data, err := b.fn()
		if err != nil {
			t.Fatalf("build %s: %v", b.name, err)
		}
		if err := os.WriteFile(filepath.Join(outDir, b.name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", b.name, err)
		}
		// Every upstream TLS context must pin the SERVER identity. Envoy
		// ACCEPTS an unpinned one — the handshake then proves only trust-domain
		// membership, which any mesh workload satisfies — so `--mode validate`
		// passing says nothing about it (issue #832). Checked on the same bytes
		// the validate below reads.
		unpinned, err := UnpinnedMeshClusters(data)
		if err != nil {
			t.Fatalf("SAN-pin check %s: %v", b.name, err)
		}
		if len(unpinned) > 0 {
			t.Errorf("%s: upstream TLS contexts with no match_typed_subject_alt_names: %v\n"+
				"an unpinned context authenticates ANY workload in the trust domain, not the service asked for", b.name, unpinned)
		}
		// The inbound half of the same property (issue #843): a chain that
		// REQUIRES a client certificate must also say what shape that
		// certificate has to be. Without it the handshake proves only that the
		// trust bundle signed something, and the XFCC the HCM stamps
		// SANITIZE_SET from that certificate carries no more weight than the
		// bundle does. Envoy accepts the unpinned form, so this too is checked
		// on the bytes rather than by the validate below.
		unpinnedIn, err := UnpinnedInboundChains(data)
		if err != nil {
			t.Fatalf("inbound SAN-pin check %s: %v", b.name, err)
		}
		if len(unpinnedIn) > 0 {
			t.Errorf("%s: mTLS-terminating filter chains with no client match_typed_subject_alt_names: %v\n"+
				"require_client_certificate alone accepts any certificate the trust bundle signs, including non-workload identities", b.name, unpinnedIn)
		}
	}

	// Validate each bootstrap with Envoy.
	for _, b := range builders {
		b := b
		t.Run(b.name, func(t *testing.T) {
			path := filepath.Join(outDir, b.name)
			cmd := exec.Command(envoy, "--mode", "validate", "-c", path)
			out, err := cmd.CombinedOutput()
			t.Logf("envoy --mode validate %s:\n%s", b.name, out)
			if err != nil {
				t.Fatalf("envoy --mode validate failed for %s: %v", b.name, err)
			}
		})
	}
}

// TestNodeBootstrapEgressRDSInitialFetchTimeout reads the SERIALISED node
// bootstrap — the same bytes `envoy --mode validate` above loads — and asserts
// the egress listener's RDS config source states its initial_fetch_timeout
// (issue #817).
//
// The field bounds how long the listener stays warming for the first out_http
// delivery; when it expires Envoy activates the listener regardless, with an
// unresolved route table, which is the 404 NR route_not_found window #817 is
// about. Envoy's default happens to be the same 15s, so this is a pin rather
// than a behaviour change: an unstated value is one that can drift under the
// mesh without any test noticing.
//
// This asserts through protojson rather than the builder's return value on
// purpose — a field lost in marshalling (or stripped alongside the custom
// filters) would still pass an in-memory check.
func TestNodeBootstrapEgressRDSInitialFetchTimeout(t *testing.T) {
	data, err := NodeBootstrapJSON()
	if err != nil {
		t.Fatalf("NodeBootstrapJSON: %v", err)
	}

	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal node bootstrap: %v", err)
	}

	var found int
	for _, l := range bs.GetStaticResources().GetListeners() {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				// Matched on the typed config's type URL, not the filter name:
				// the HCM is emitted under the deprecated "envoy.http_connection_manager"
				// alias, and a name check would silently find nothing.
				hcm := &http_connection_managerv3.HttpConnectionManager{}
				if err := f.GetTypedConfig().UnmarshalTo(hcm); err != nil {
					continue
				}
				rds := hcm.GetRds()
				if rds == nil || rds.GetRouteConfigName() != proxy.OutboundHTTPRouteName {
					continue
				}
				found++
				ift := rds.GetConfigSource().GetInitialFetchTimeout()
				if ift == nil {
					t.Fatalf("listener %q: out_http RDS config source has no initial_fetch_timeout", l.GetName())
				}
				if got := ift.AsDuration(); got != proxy.OutboundRouteInitialFetchTimeout {
					t.Fatalf("listener %q: out_http RDS initial_fetch_timeout = %s, want %s",
						l.GetName(), got, proxy.OutboundRouteInitialFetchTimeout)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no listener in the node bootstrap references the out_http route config over RDS")
	}
}

// TestNodeBootstrapCarriesPerConnectionCertSelector proves the `envoy --mode
// validate` gate above is not VACUOUS for issue #842.
//
// The validated bootstrap must actually contain the on-demand certificate
// selector and its filter-state mapper. `envoy --mode validate` instantiates
// both factories and PGV-validates their configs, so an extension missing from
// the pinned proxy build, or a config the proto rejects, is a validation
// FAILURE — which is the whole value of running a real Envoy over this config.
// But that value is zero if the fixture stopped emitting the selector: the gate
// would pass on config that does not exercise it, exactly the trap the
// shell-lint job fell into (aether#853).
//
// MEASURED, both ways, on the pinned proxy (2026-09-20):
//   - emptying filter_state_override's default_value makes validation FAIL with
//     "ConfigValidationError.DefaultValue: value length must be at least 1
//     characters" — so the gate really does reach inside the selector;
//   - corrupting the TypedExtensionConfig's `name` does NOT fail. Extensions
//     here resolve by the typed_config TYPE URL, and the name is informational.
//     Do not rely on validation to catch a renamed constant; that is what
//     TestUpstreamCertSelectorExtensionNames (agent/internal/xds/proxy) is for.
//
// So: assert the shape is present, in the marshalled bytes, before trusting
// that validation accepting them means anything.
func TestNodeBootstrapCarriesPerConnectionCertSelector(t *testing.T) {
	data, err := NodeBootstrapJSON()
	if err != nil {
		t.Fatalf("NodeBootstrapJSON: %v", err)
	}

	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal node bootstrap: %v", err)
	}

	var checked int
	for _, c := range bs.GetStaticResources().GetClusters() {
		ts := c.GetTransportSocket()
		if ts == nil {
			continue
		}
		utc := &tlsv3.UpstreamTlsContext{}
		if err := ts.GetTypedConfig().UnmarshalTo(utc); err != nil {
			continue
		}
		sel := utc.GetCommonTlsContext().GetCustomTlsCertificateSelector()
		if sel == nil {
			continue
		}
		checked++

		onDemand := &on_demand_secretv3.Config{}
		if err := sel.GetTypedConfig().UnmarshalTo(onDemand); err != nil {
			t.Fatalf("cluster %q: custom_tls_certificate_selector is not an on_demand_secret config: %v", c.GetName(), err)
		}
		if onDemand.GetConfigSource() == nil {
			t.Fatalf("cluster %q: on_demand_secret has no config_source (required)", c.GetName())
		}
		mapper := &filter_state_overridev3.Config{}
		if err := onDemand.GetCertificateMapper().GetTypedConfig().UnmarshalTo(mapper); err != nil {
			t.Fatalf("cluster %q: certificate_mapper is not a filter_state_override config: %v", c.GetName(), err)
		}
		if mapper.GetDefaultValue() == "" {
			t.Fatalf("cluster %q: filter_state_override default_value is empty (min_len: 1 — Envoy would reject it)", c.GetName())
		}
		if got := utc.GetMaxSessionKeys().GetValue(); got != 0 {
			t.Fatalf("cluster %q: max_session_keys = %d, want 0 — a client context supports a custom certificate selector only with session resumption off", c.GetName(), got)
		}
	}
	if checked == 0 {
		t.Fatal("no cluster in the node bootstrap carries a per-connection certificate selector: the envoy --mode validate gate proves nothing about issue #842")
	}
}
