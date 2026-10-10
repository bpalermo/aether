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
//	bazel test //agent/test/envoy_validate:envoy_validate_test
//	bazel test //agent/test/envoy_validate:envoy_validate_test --test_output=all
//
// What the test catches (examples from production incidents):
//   - ORIGINAL_DST cluster with ROUND_ROBIN lb_policy      → CDS NACK, exit 1
//   - Listener with no address field                        → LDS NACK, exit 1
//   - Malformed / missing SAN in TLS validation context     → config rejected
//   - Unknown TypedConfig @type URL (non-stripped filter)   → config rejected
package envoy_validate

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/wrapperspb"
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

	// Say which Envoy this run validated against, and refuse anything that is
	// not one. The repo can be swapped for a locally built proxy
	// (validate-built-proxy.sh, --override_repository), so the log line is the
	// CI evidence of which binary the gate ran, and the identity check keeps a
	// stand-in that exits 0 (/bin/true) from passing every positive case.
	id := identity(p)
	if id.err != nil {
		t.Fatalf("%s is not a working Envoy: %v", id.resolved, id.err)
	}
	t.Logf("envoy under validation: %s (%s)", id.resolved, id.version)
	return p
}

type envoyIdentity struct {
	resolved string
	version  string
	err      error
}

// identity runs `envoy --version` once per binary per test process.
var identity = func() func(string) envoyIdentity {
	var mu sync.Mutex
	seen := map[string]envoyIdentity{}
	return func(p string) envoyIdentity {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := seen[p]; ok {
			return id
		}
		id := envoyIdentity{resolved: p}
		// The runfiles entry is a symlink into the repo, and an overridden
		// repo may symlink further, to the build output the gate was pointed
		// at: log the end of that chain, not the runfiles alias.
		if r, err := filepath.EvalSymlinks(p); err == nil {
			id.resolved = r
		}
		id.version, id.err = envoyVersion(p)
		seen[p] = id
		return id
	}
}()

// envoyVersion returns the `envoy --version` line of bin, or an error when bin
// does not run, exits non-zero, or prints no Envoy version line.
func envoyVersion(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w\n%s", bin, err, out)
	}
	for line := range strings.Lines(string(out)) {
		if _, v, ok := strings.Cut(line, " version: "); ok && strings.TrimSpace(v) != "" {
			return "version: " + strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("%s --version printed no Envoy version line:\n%s", bin, out)
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
		{"capture_tcproute_bootstrap.json", CaptureTCPRouteBootstrapJSON},
		{"capture_tlsroute_bootstrap.json", CaptureTLSRouteBootstrapJSON},
		{"capture_udp_bootstrap.json", CaptureUDPBootstrapJSON},
		{"edge_bootstrap.json", EdgeBootstrapJSON},
		{"quic_outbound_bootstrap.json", QUICOutboundBootstrapJSON},
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
		// No cluster but a service's default may subscribe to an EDS name that
		// is not its own (aether#842/#1008/#1013): a later-added sharer is
		// deduplicated by the delta-ADS WatchMap into 15 s of warming.
		sharing, err := ClustersSharingServiceEDSName(data)
		if err != nil {
			t.Fatalf("EDS-name check %s: %v", b.name, err)
		}
		if len(sharing) > 0 {
			t.Errorf("%s: clusters not subscribed to an EDS name of their own: %v", b.name, sharing)
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
		// Proposal 038 R4: every mTLS QUIC chain must EXPLICITLY disable
		// session resumption and 0-RTT. Envoy accepts the unset form and, at
		// this pin, resumes without re-verifying the client certificate.
		noR4, err := QUICChainsWithoutR4(data)
		if err != nil {
			t.Fatalf("R4 check %s: %v", b.name, err)
		}
		if len(noR4) > 0 {
			t.Errorf("%s: mTLS QUIC chains without explicit enable_resumption:false + enable_early_data:false: %v\n"+
				"a resumed or 0-RTT QUIC session carries a peer identity the destination never verified (038 R4)", b.name, noR4)
		}
		// R4, client side: every QUIC upstream must explicitly disable the
		// client session cache (max_session_keys: 0).
		cached, err := QUICUpstreamsWithSessionCache(data)
		if err != nil {
			t.Fatalf("QUIC upstream R4 check %s: %v", b.name, err)
		}
		if len(cached) > 0 {
			t.Errorf("%s: QUIC upstream clusters without explicit max_session_keys:0: %v (038 R4)", b.name, cached)
		}
		// aether#1023: every L4 cluster reports under its OWN kind-prefixed
		// stat key, shared with no other cluster. Envoy accepts any
		// alt_stat_name, so only the bytes can say.
		badKeys, _, err := L4StatKeyViolations(data)
		if err != nil {
			t.Fatalf("L4 stat-key check %s: %v", b.name, err)
		}
		if len(badKeys) > 0 {
			t.Errorf("%s: L4 clusters not reporting under their own tcp_/udp_ key: %v\n"+
				"a shared key merges L4 kinds (and the HTTP cluster) into one aether_cluster series, and a verify_san tick can no longer be attributed (#1007)", b.name, badKeys)
		}
		// aether#1023: every tcp_proxy chain on a capture listener carries the
		// connection-level L4 access log.
		unlogged, _, err := CaptureTCPChainsWithoutL4AccessLog(data)
		if err != nil {
			t.Fatalf("L4 access-log check %s: %v", b.name, err)
		}
		if len(unlogged) > 0 {
			t.Errorf("%s: capture L4 chains without the %s access log: %v", b.name, proxy.L4AccessLogName, unlogged)
		}
		// aether#1165: the pre-#842 identity key is retired. No listener may
		// stamp it, log it, or match on it -- every reader moved to
		// proxy.SourceIdentityCertMapperFilterStateKey.
		retired, nListeners, err := ListenersNamingRetiredSourceKey(data)
		if err != nil {
			t.Fatalf("retired-key check %s: %v", b.name, err)
		}
		if nListeners == 0 {
			t.Errorf("%s: no listeners decoded; the retired-key check is vacuous", b.name)
		}
		if len(retired) > 0 {
			t.Errorf("%s: listeners still naming the retired %q filter-state key: %v", b.name, RetiredSourceIdentityKey, retired)
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
		// The selector must NOT ride `ads: {}` (issue #842, the rev228 outage):
		// every secret it asks for is already subscribed on that mux by a static
		// reference, Envoy's delta WatchMap deduplicates the interest away, and
		// the handshake then pauses forever with no error and no stat. It gets
		// its own api_config_source — and that source must name a cluster this
		// bootstrap actually defines, or the reference dangles.
		if onDemand.GetConfigSource().GetAds() != nil {
			t.Fatalf("cluster %q: the certificate selector fetches over the shared ADS stream; "+
				"it needs its own api_config_source (issue #842)", c.GetName())
		}
		api := onDemand.GetConfigSource().GetApiConfigSource()
		if api == nil || len(api.GetGrpcServices()) != 1 {
			t.Fatalf("cluster %q: the certificate selector needs exactly one explicit gRPC config source", c.GetName())
		}
		backing := api.GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName()
		if !staticClusterNames(bs)[backing] {
			t.Fatalf("cluster %q: the certificate selector's config source names cluster %q, "+
				"which this bootstrap does not define — a dangling SDS reference", c.GetName(), backing)
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

// staticClusterNames is the set of clusters a bootstrap defines statically, for
// checking that a config source's backing cluster actually resolves.
func staticClusterNames(bs *bootstrapv3.Bootstrap) map[string]bool {
	names := make(map[string]bool, len(bs.GetStaticResources().GetClusters()))
	for _, c := range bs.GetStaticResources().GetClusters() {
		names[c.GetName()] = true
	}
	return names
}

// ---------------------------------------------------------------------------
// L4 routes: TCPRoute / TLSRoute / UDPRoute (proposal 018 Phase 3b, issue #868)
// ---------------------------------------------------------------------------
//
// `envoy --mode validate` above already runs over the three L4 bootstraps, and
// that is not nothing — it is the only thing that certifies a connection-less
// UDP listener's shape, and the only thing that certifies two SNI chains and a
// floor chain matching the same /32 are not ambiguous to a real Envoy.
//
// But validation is fail-open about everything it does not have an opinion on,
// and it is entirely satisfied by a fixture that stopped emitting the thing
// under test. So each leg gets a structural assertion over the SERIALISED
// bytes, for the same reason TestNodeBootstrapCarriesPerConnectionCertSelector
// does: a gate that cannot distinguish "correct" from "absent" is the shape
// aether#853 already shipped once.

// tcpProxyByChain returns every filter chain's tcp_proxy config in a listener,
// keyed by chain name, alongside the chains themselves.
//
// Matched on the typed config's TYPE URL rather than the filter name: the same
// reason TestNodeBootstrapEgressRDSInitialFetchTimeout gives for the HCM.
func tcpProxyByChain(t *testing.T, l *listenerv3.Listener) map[string]*tcp_proxyv3.TcpProxy {
	t.Helper()
	out := make(map[string]*tcp_proxyv3.TcpProxy)
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			tp := &tcp_proxyv3.TcpProxy{}
			if err := f.GetTypedConfig().UnmarshalTo(tp); err != nil {
				continue
			}
			out[fc.GetName()] = tp
		}
	}
	return out
}

// singleListener unmarshals a bootstrap and returns its one static listener.
func singleListener(t *testing.T, data []byte) *listenerv3.Listener {
	t.Helper()
	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	ls := bs.GetStaticResources().GetListeners()
	if len(ls) != 1 {
		t.Fatalf("bootstrap has %d static listeners, want exactly 1", len(ls))
	}
	return ls[0]
}

// TestCaptureTCPRouteWeightedFloorChain asserts the TCPRoute fixture's floor
// chain really carries a WEIGHTED tcp_proxy over both backends at the weights
// the route asked for.
//
// Why this and not just `--mode validate`: Envoy accepts a single-cluster
// tcp_proxy just as happily as a weighted one, so validation passing says
// nothing about whether the weighting survived. And weighting is exactly where
// this area has shipped bugs — #492 turned an explicit `weight: 0` (DRAIN,
// per Gateway API) into an equal share by normalising it to 1, which every
// structural check of the day accepted.
func TestCaptureTCPRouteWeightedFloorChain(t *testing.T) {
	data, err := CaptureTCPRouteBootstrapJSON()
	if err != nil {
		t.Fatalf("CaptureTCPRouteBootstrapJSON: %v", err)
	}
	l := singleListener(t, data)

	proxies := tcpProxyByChain(t, l)
	var floor *tcp_proxyv3.TcpProxy
	var floorName string
	for name, tp := range proxies {
		if strings.HasPrefix(name, "cap_tcp_") {
			floor, floorName = tp, name
		}
	}
	if floor == nil {
		t.Fatalf("no cap_tcp_* filter chain in listener %q: chains %v", l.GetName(), chainNames(l))
	}

	wc := floor.GetWeightedClusters()
	if wc == nil {
		t.Fatalf("chain %q: tcp_proxy has cluster_specifier %T, want weighted_clusters — "+
			"a TCPRoute with two live backends must not collapse to a single cluster",
			floorName, floor.GetClusterSpecifier())
	}
	got := make(map[string]uint32, len(wc.GetClusters()))
	for _, c := range wc.GetClusters() {
		got[c.GetName()] = c.GetWeight()
	}
	// The expected weights are LITERALS, not the L4TCPWeight* constants the
	// fixture is built from. Reading the expectation back out of the same
	// constant that produced it is a gate that cannot fail: measured on
	// 2026-09-20, editing L4TCPWeightA from 75 to 50 left this test GREEN
	// until these two numbers were spelled out here. That is the aether#853
	// shape in miniature, inside the test written to avoid it.
	want := map[string]uint32{
		L4TCPBackendClusterA(): 75,
		L4TCPBackendClusterB(): 25,
	}
	if len(got) != len(want) {
		t.Fatalf("chain %q: weighted_clusters = %v, want %v", floorName, got, want)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("chain %q: cluster %q weight = %d, want %d (weighted_clusters: %v)",
				floorName, name, got[name], w, got)
		}
	}

	// Every weighted cluster must be defined by this bootstrap. A weighted
	// reference to a cluster nobody emits is the failure mode the plan calls
	// out for the e2e harness (there is no ODCDS for tcp_proxy: the chain
	// matches and the cluster is simply missing), and `--mode validate` does
	// NOT catch it — tcp_proxy resolves its cluster at connection time.
	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	defined := staticClusterNames(bs)
	for name := range got {
		if !defined[name] {
			t.Errorf("chain %q routes to cluster %q, which the bootstrap does not define — "+
				"tcp_proxy has no on-demand CDS, so this chain matches and then has nowhere to send the connection",
				floorName, name)
		}
	}
}

// TestCaptureTCPRouteDrainCollapsesToSingleCluster asserts the shape a DRAIN
// produces: with one backend at weight 0 the weighted set has one member left,
// and buildWeightedTCPProxy emits the simpler single-cluster form.
//
// This is the #492 property at the config layer. The drained backend must be
// ABSENT, not present at weight 0 and not normalised to 1; asserting the
// collapsed form is stronger than asserting "two entries, one of them zero",
// because the zero-weight entry is precisely what the pre-#492 code could not
// produce either.
//
// It calls the builder directly rather than going through a bootstrap: the
// interesting output is one filter chain, and a fourth bootstrap would add an
// Envoy process to the test for no extra coverage.
func TestCaptureTCPRouteDrainCollapsesToSingleCluster(t *testing.T) {
	svc := proxy.CaptureTCPService{
		ClusterName: "tcp:l4-front.default.mesh.local",
		ClusterIP:   L4TCPParentClusterIP,
		// TCP-primary: these fixtures exercise the TCP floor, which since
		// proposal 037 design (d) is emitted only for a TCP-primary service.
		PrimaryIsTCP: true,
	}
	rules := []proxy.L4ServiceRoute{{
		Backends: []proxy.L4Backend{
			{Service: "default/l4-a", Cluster: L4TCPBackendClusterA(), Weight: 100},
			// Explicit 0 = DRAIN. The reconciler defaults an UNSET weight to 1,
			// so a 0 arriving here is always deliberate.
			{Service: "default/l4-b", Cluster: L4TCPBackendClusterB(), Weight: 0},
		},
	}}

	fc := proxy.BuildCaptureTCPRouteFilterChain(svc, rules, "spiffe://aether.internal/ns/default/sa/default")
	if fc == nil {
		t.Fatal("BuildCaptureTCPRouteFilterChain returned nil for a route with one live backend")
	}

	var tp *tcp_proxyv3.TcpProxy
	for _, f := range fc.GetFilters() {
		c := &tcp_proxyv3.TcpProxy{}
		if err := f.GetTypedConfig().UnmarshalTo(c); err == nil {
			tp = c
		}
	}
	if tp == nil {
		t.Fatalf("chain %q carries no tcp_proxy filter", fc.GetName())
	}

	if wc := tp.GetWeightedClusters(); wc != nil {
		names := make([]string, 0, len(wc.GetClusters()))
		for _, c := range wc.GetClusters() {
			names = append(names, fmt.Sprintf("%s=%d", c.GetName(), c.GetWeight()))
		}
		t.Fatalf("one backend drained (weight 0) but tcp_proxy still carries weighted_clusters %v; "+
			"weight 0 means DRAIN, not an equal share (#492)", names)
	}
	if got, want := tp.GetCluster(), L4TCPBackendClusterA(); got != want {
		t.Errorf("tcp_proxy cluster = %q, want %q — the surviving backend", got, want)
	}
}

// TestCaptureTLSRouteSNIChainsCoexistWithFloor asserts the TLSRoute listener's
// three-chain shape, which is what makes the fall-through in #868 a designed
// outcome rather than an accident:
//
//   - each cap_tls_*_<i> chain matches prefix_ranges <ClusterIP>/32 AND
//     server_names — the /32 is what stops a TLSRoute for service A claiming
//     service B's connections, since a client can set any SNI it likes;
//   - the cap_tcp_* floor chain matches the SAME /32 with NO server_names, so a
//     connection whose SNI matches nothing lands there.
//
// The second half is the one worth stating out loud. #868 was first written as
// "a non-matching SNI does not fall through to a default", which is the
// opposite of what the code does; asserting that would have pinned a bug. Envoy
// selects filter chains by specificity, so an unconstrained-SNI chain is the
// deliberate default. This test asserts the fall-through POSITIVELY: the floor
// chain exists, shares the /32, sets no server_names, and routes to the
// PARENT's own cluster rather than to either TLSRoute backend.
func TestCaptureTLSRouteSNIChainsCoexistWithFloor(t *testing.T) {
	data, err := CaptureTLSRouteBootstrapJSON()
	if err != nil {
		t.Fatalf("CaptureTLSRouteBootstrapJSON: %v", err)
	}
	l := singleListener(t, data)
	proxies := tcpProxyByChain(t, l)

	// The SNI chains, by the hostname each one claims.
	backendBySNI := map[string]string{}
	var floorName string
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		switch {
		case strings.HasPrefix(fc.GetName(), "cap_tls_"):
			if len(m.GetServerNames()) != 1 {
				t.Errorf("chain %q: server_names = %v, want exactly one hostname", fc.GetName(), m.GetServerNames())
				continue
			}
			assertMatchesParentIP(t, fc, L4TLSParentClusterIP,
				"without the /32 an SNI chain would claim any service's connection carrying that hostname")
			backendBySNI[m.GetServerNames()[0]] = proxies[fc.GetName()].GetCluster()
		case strings.HasPrefix(fc.GetName(), "cap_tcp_"):
			floorName = fc.GetName()
			if len(m.GetServerNames()) != 0 {
				t.Errorf("floor chain %q constrains server_names to %v; it must not, or a "+
					"connection whose SNI matches no TLSRoute has nowhere to land",
					fc.GetName(), m.GetServerNames())
			}
			assertMatchesParentIP(t, fc, L4TLSParentClusterIP,
				"the floor chain is the fall-through for this service's VIP")
		}
	}

	// Both SNIs are routed, and to DIFFERENT backends. Asserting only one SNI
	// would pass just as well if server_names were ignored and chain 0 always
	// won, which is the mirror-assertion point #868 makes for the e2e probes.
	want := map[string]string{
		L4SNIAlpha: L4TLSBackendClusterA(),
		L4SNIBravo: L4TLSBackendClusterB(),
	}
	if len(backendBySNI) != len(want) {
		t.Fatalf("SNI chains = %v, want one chain per hostname %v", backendBySNI, want)
	}
	for sni, cluster := range want {
		if backendBySNI[sni] != cluster {
			t.Errorf("SNI %q routes to %q, want %q", sni, backendBySNI[sni], cluster)
		}
	}

	// The fall-through target, positively: the floor goes to the parent's own
	// cluster, which is neither TLSRoute backend.
	if floorName == "" {
		t.Fatalf("no cap_tcp_* floor chain beside the SNI chains: chains %v", chainNames(l))
	}
	floorCluster := proxies[floorName].GetCluster()
	if floorCluster != L4TLSParentFloorCluster() {
		t.Errorf("floor chain %q routes to %q, want the parent's own cluster %q",
			floorName, floorCluster, L4TLSParentFloorCluster())
	}
	for sni, backend := range want {
		if floorCluster == backend {
			t.Errorf("floor chain routes to %q, the backend of SNI %q — a non-matching SNI would "+
				"reach a TLSRoute backend", backend, sni)
		}
	}
}

// assertMatchesParentIP checks a chain's filter_chain_match pins the parent
// Service's ClusterIP as a /32.
func assertMatchesParentIP(t *testing.T, fc *listenerv3.FilterChain, ip, why string) {
	t.Helper()
	ranges := fc.GetFilterChainMatch().GetPrefixRanges()
	if len(ranges) != 1 || ranges[0].GetAddressPrefix() != ip || ranges[0].GetPrefixLen().GetValue() != 32 {
		t.Errorf("chain %q: prefix_ranges = %v, want exactly %s/32 — %s", fc.GetName(), ranges, ip, why)
	}
}

// chainNames lists a listener's filter chain names, for failure messages.
func chainNames(l *listenerv3.Listener) []string {
	out := make([]string, 0, len(l.GetFilterChains()))
	for _, fc := range l.GetFilterChains() {
		out = append(out, fc.GetName())
	}
	return out
}

// TestCaptureUDPListenerIsConnectionless asserts the properties that make a
// UDPRoute listener work at all, none of which any other test covers.
//
//  1. NO filter_chains. Envoy rejects a connection-less UDP listener that has
//     any ("N filter chain(s) specified for connection-less UDP listener"), so
//     the udp_proxy config must ride listener_filters instead. l4route.go
//     records that rule in a comment; the `--mode validate` run beside this
//     test is what turns a regression here into a failure rather than a
//     surprise on a node.
//  2. The listener binds UDP, in the pod netns, on the L4 MESH port, and is
//     transparent (proposal 038).
//  3. SELECTION: the udp_proxy route specifier is a matcher on the dialled
//     destination IP with one arm per UDPRoute parent, each naming a cluster
//     the bootstrap defines. Before 038 the listener carried one cluster for
//     the whole node and there was, by design, no selection assertion here.
func TestCaptureUDPListenerIsConnectionless(t *testing.T) {
	data, err := CaptureUDPBootstrapJSON()
	if err != nil {
		t.Fatalf("CaptureUDPBootstrapJSON: %v", err)
	}
	l := singleListener(t, data)

	if n := len(l.GetFilterChains()); n != 0 {
		t.Errorf("UDP listener %q carries %d filter_chains; a connection-less UDP listener must carry none "+
			"(Envoy: \"%d filter chain(s) specified for connection-less UDP listener\")", l.GetName(), n, n)
	}
	if l.GetDefaultFilterChain() != nil {
		t.Errorf("UDP listener %q carries a default_filter_chain; same rule as filter_chains", l.GetName())
	}

	sa := l.GetAddress().GetSocketAddress()
	if sa.GetProtocol() != corev3.SocketAddress_UDP {
		t.Errorf("UDP listener %q binds protocol %v, want UDP", l.GetName(), sa.GetProtocol())
	}
	// 18082 as a literal, on purpose: this is a CROSS-TREE pin, not a
	// restatement of the fixture. The CNI's divert marks udp dport 18082 with
	// NO port rewrite (a datagram reply's source port is the socket's bound
	// port, so the listener must bind the port the client dialled), so the
	// agent and the CNI plugin have to agree on the number. Comparing the
	// listener against the same meshconst the fixture passed in would be a
	// check that cannot fail (see the note on the TCPRoute weights).
	if got := sa.GetPortValue(); got != 18082 || meshconst.ProxyL4OutboundPort != 18082 {
		t.Errorf("UDP listener %q binds port %d (meshconst.ProxyL4OutboundPort = %d), want 18082 — "+
			"the CNI diverts that port with no rewrite and Envoy replies from the bound port",
			l.GetName(), got, meshconst.ProxyL4OutboundPort)
	}
	if !l.GetTransparent().GetValue() {
		t.Errorf("UDP listener %q is not transparent: the divert delivers a datagram addressed to a non-local VIP, "+
			"which only an IP_TRANSPARENT socket receives, and the reply must leave from that VIP", l.GetName())
	}
	if sa.GetNetworkNamespaceFilepath() == "" {
		t.Errorf("UDP listener %q has no network_namespace_filepath; it would bind in the agent's netns, not the pod's", l.GetName())
	}

	// The udp_proxy config lives in listener_filters and is a matcher on the
	// destination IP with one arm per parent.
	var cfg *udp_proxyv3.UdpProxyConfig
	for _, lf := range l.GetListenerFilters() {
		c := &udp_proxyv3.UdpProxyConfig{}
		if err := lf.GetTypedConfig().UnmarshalTo(c); err == nil {
			cfg = c
		}
	}
	if cfg == nil {
		t.Fatalf("UDP listener %q has no udp_proxy listener filter: the listener would receive datagrams and drop them", l.GetName())
	}
	if cfg.GetCluster() != "" {
		t.Errorf("udp_proxy uses the deprecated single `cluster` specifier (%q); 038 keys a matcher on the dialled VIP", cfg.GetCluster())
	}
	tree := cfg.GetMatcher().GetMatcherTree()
	if tree == nil {
		t.Fatalf("udp_proxy has no matcher_tree; without it every parent but one is dropped (#873)")
	}
	in := &network_inputsv3.DestinationIPInput{}
	if err := tree.GetInput().GetTypedConfig().UnmarshalTo(in); err != nil {
		t.Fatalf("matcher input is not DestinationIPInput: %v — the only thing that distinguishes two parents is the VIP the pod dialled", err)
	}
	arms := map[string]string{}
	for vip, om := range tree.GetExactMatchMap().GetMap() {
		r := &udp_proxyv3.Route{}
		if err := om.GetAction().GetTypedConfig().UnmarshalTo(r); err != nil {
			t.Fatalf("arm %s: action is not a udp_proxy Route: %v", vip, err)
		}
		arms[vip] = r.GetCluster()
	}
	want := map[string]string{
		L4UDPParentClusterIPA: L4UDPBackendClusterA(),
		L4UDPParentClusterIPB: L4UDPBackendClusterB(),
	}
	if len(arms) != len(want) {
		t.Fatalf("udp_proxy carries %d arm(s), want %d: %v", len(arms), len(want), arms)
	}
	for vip, cluster := range want {
		if got := arms[vip]; got != cluster {
			t.Errorf("datagrams to %s route to %q, want %q: the wrong parent's backend would receive them", vip, got, cluster)
		}
	}

	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	defined := staticClusterNames(bs)
	for vip, cluster := range arms {
		if !defined[cluster] {
			t.Fatalf("arm %s names cluster %q, which the bootstrap does not define", vip, cluster)
		}
	}
}

// TestCaptureUDPClusterIsPlaintextAtTheAppPort asserts the concrete shape of
// "UDP rides the mesh in plaintext" (#868), which is otherwise only a comment.
//
// mTLS is a TCP/TLS construct and DTLS is not implemented, so the UDP floor has
// no inbound mesh hop: udp_proxy dials the backend pod's APPLICATION UDP port
// directly, over a STATIC cluster with an inline load assignment and NO
// transport socket. Each of those three is load-bearing:
//
//   - a transport socket appearing here would be a silent claim of encryption
//     the data path does not provide;
//   - an endpoint left on the mesh inbound port (:18008) would send datagrams
//     at a TCP mTLS listener, which would drop them;
//   - TCP-protocol endpoints would not carry UDP at all.
//
// This is the structural assertion the e2e harness deliberately does NOT
// duplicate at the wire level (a packet capture proving "not TLS" would be
// decoration); the plan puts it here on purpose.
func TestCaptureUDPClusterIsPlaintextAtTheAppPort(t *testing.T) {
	data, err := CaptureUDPBootstrapJSON()
	if err != nil {
		t.Fatalf("CaptureUDPBootstrapJSON: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}

	var udp *clusterv3.Cluster
	for _, c := range bs.GetStaticResources().GetClusters() {
		if c.GetName() == L4UDPBackendClusterA() {
			udp = c
		}
	}
	if udp == nil {
		t.Fatalf("bootstrap defines no %q cluster", L4UDPBackendClusterA())
	}

	if ts := udp.GetTransportSocket(); ts != nil {
		t.Errorf("cluster %q carries transport_socket %q; the UDP floor is PLAINTEXT — mesh mTLS is a "+
			"TCP/TLS construct and DTLS is not implemented, so a socket here would claim protection "+
			"the data path does not provide", udp.GetName(), ts.GetName())
	}
	if len(udp.GetTransportSocketMatches()) != 0 {
		t.Errorf("cluster %q carries transport_socket_matches; same reason", udp.GetName())
	}
	if got := udp.GetType(); got != clusterv3.Cluster_STATIC {
		t.Errorf("cluster %q type = %v, want STATIC with an inline load assignment: the shared bare-name "+
			"EDS resource carries mesh INBOUND endpoints, which is the wrong port for UDP", udp.GetName(), got)
	}

	var endpoints int
	for _, lle := range udp.GetLoadAssignment().GetEndpoints() {
		for _, lb := range lle.GetLbEndpoints() {
			sa := lb.GetEndpoint().GetAddress().GetSocketAddress()
			if sa == nil {
				t.Errorf("cluster %q: endpoint has no socket address", udp.GetName())
				continue
			}
			endpoints++
			if sa.GetProtocol() != corev3.SocketAddress_UDP {
				t.Errorf("cluster %q: endpoint %s protocol = %v, want UDP",
					udp.GetName(), sa.GetAddress(), sa.GetProtocol())
			}
			if got, want := sa.GetPortValue(), uint32(L4UDPBackendPort); got != want {
				t.Errorf("cluster %q: endpoint %s port = %d, want the backend's APPLICATION port %d — "+
					"the UDP floor has no inbound mTLS hop to dial",
					udp.GetName(), sa.GetAddress(), got, want)
			}
		}
	}
	if endpoints == 0 {
		t.Fatalf("cluster %q has no endpoints: the assertions above checked nothing", udp.GetName())
	}
}

// TestNodeBootstrapCarriesTheQUICInbound is the anti-vacuity half of the R4
// and inbound-pin checks above: the node bootstrap must contain the pod's
// HTTP/3 inbound (inbound_<namespace>_<pod>_h3), on UDP, on the TCP inbound's port, with
// a QuicDownstreamTransport that requires a client certificate -- otherwise
// QUICChainsWithoutR4 and the QUIC branch of downstreamTLSPinned are checking
// nothing when they pass.
func TestNodeBootstrapCarriesTheQUICInbound(t *testing.T) {
	data, err := NodeBootstrapJSON()
	if err != nil {
		t.Fatalf("NodeBootstrapJSON: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	var tcpPort uint32
	var quic *listenerv3.Listener
	for _, l := range bs.GetStaticResources().GetListeners() {
		switch {
		case strings.HasSuffix(l.GetName(), "_h3") && strings.HasPrefix(l.GetName(), "inbound_"):
			quic = l
		case strings.HasPrefix(l.GetName(), "inbound_"):
			tcpPort = l.GetAddress().GetSocketAddress().GetPortValue()
		}
	}
	if quic == nil {
		t.Fatal("the node bootstrap has no inbound_<namespace>_<pod>_h3 listener: the QUIC checks above are vacuous")
	}
	sa := quic.GetAddress().GetSocketAddress()
	if sa.GetProtocol() != corev3.SocketAddress_UDP {
		t.Errorf("QUIC inbound binds %v, want UDP", sa.GetProtocol())
	}
	// 18008 as a literal on purpose: a CROSS-TREE pin (038 R3), not a
	// restatement of whatever the builder used -- the source side dials the
	// number the TCP inbound advertises, and QUIC must be reachable at exactly
	// that number or a second port gets allocated by accident.
	if sa.GetPortValue() != 18008 || tcpPort != 18008 {
		t.Errorf("QUIC inbound on %d, TCP inbound on %d, want both on 18008 (038 R3: the inbound port number is shared across transports)", sa.GetPortValue(), tcpPort)
	}
	if !quic.GetEnableReusePort().GetValue() {
		t.Errorf("QUIC inbound lacks enable_reuse_port; a QUIC listener needs it for per-worker sockets")
	}
	if quic.GetUdpListenerConfig().GetQuicOptions() == nil {
		t.Errorf("QUIC inbound has no udp_listener_config.quic_options; without it this is a plain UDP listener")
	}
	// aether#1021: GRO on the receive path (Envoy defaults it OFF for listener
	// sockets; measured -11 % destination CPU per request), and the send path
	// left to Envoy's automatic writer, which is the GSO batch writer wherever
	// the kernel supports UDP_SEGMENT. A writer named here would either drop
	// GSO (the default writer) or drop the kernel-support check (the explicit
	// GSO writer).
	if !quic.GetUdpListenerConfig().GetDownstreamSocketConfig().GetPreferGro().GetValue() {
		t.Errorf("QUIC inbound does not set udp_listener_config.downstream_socket_config.prefer_gro: true (aether#1021)")
	}
	if w := quic.GetUdpListenerConfig().GetUdpPacketPacketWriterConfig(); w != nil {
		t.Errorf("QUIC inbound pins udp_packet_packet_writer_config %q; leave it unset so Envoy picks GSO when the kernel supports it (aether#1021)", w.GetName())
	}
	var mtlsChains int
	for _, fc := range quic.GetFilterChains() {
		ctx, err := downstreamTLSContextOf(fc.GetTransportSocket())
		if err != nil {
			t.Fatalf("chain %s: %v", fc.GetName(), err)
		}
		if ctx == nil {
			t.Errorf("chain %s carries no DownstreamTlsContext inside its transport socket", fc.GetName())
			continue
		}
		if !ctx.GetRequireClientCertificate().GetValue() {
			t.Errorf("chain %s does not require a client certificate: the QUIC inbound would accept anonymous callers", fc.GetName())
		}
		if alpn := ctx.GetCommonTlsContext().GetAlpnProtocols(); len(alpn) != 1 || alpn[0] != "h3" {
			t.Errorf("chain %s ALPN = %v, want exactly [h3]: any other ALPN fails an h3 client with alert 120", fc.GetName(), alpn)
		}
		mtlsChains++
	}
	if mtlsChains == 0 {
		t.Fatal("the QUIC inbound has no filter chains")
	}
}

// TestListenersNamingRetiredSourceKeySeesThroughAny is the anti-vacuity half
// of the aether#1165 check in TestEnvoyValidate: the key it hunts for only ever
// appears inside an Any (a set_filter_state config, an access-log attribute, a
// matcher input), so a detector that saw the Any's packed bytes instead of its
// rendered fields would pass every fixture vacuously. A hand-built listener
// carrying the retired key one Any deep must be reported, and a clean
// listener beside it must not.
func TestListenersNamingRetiredSourceKeySeesThroughAny(t *testing.T) {
	addr := func(port uint32) *corev3.Address {
		return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		}}}
	}
	dirty := &listenerv3.Listener{
		Name:    "dirty",
		Address: addr(18181),
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{{
			Name:       "carrier",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(&network_inputsv3.FilterStateInput{Key: RetiredSourceIdentityKey})},
		}}}},
	}
	clean := &listenerv3.Listener{
		Name:    "clean",
		Address: addr(18182),
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{{
			Name:       "carrier",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(&network_inputsv3.FilterStateInput{Key: proxy.SourceIdentityCertMapperFilterStateKey})},
		}}}},
	}
	data, err := marshalBootstrap(newBootstrap([]*clusterv3.Cluster{xdsCluster()}, []*listenerv3.Listener{dirty, clean}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, n, err := ListenersNamingRetiredSourceKey(data)
	if err != nil {
		t.Fatalf("ListenersNamingRetiredSourceKey: %v", err)
	}
	if n != 2 || !slices.Equal(got, []string{"dirty"}) {
		t.Fatalf("got %v of %d listeners, want [dirty] of 2: the retired-key check cannot see inside an Any", got, n)
	}
}

// TestUnpinnedInboundChainsSeesThroughQUIC proves the QUIC unwrap in
// downstreamTLSPinned is not decorative: a hand-built bootstrap with ONE
// chain whose QuicDownstreamTransport requires a client certificate and pins
// nothing must be reported. Before the unwrap this passed vacuously (the typed
// config was "not a DownstreamTlsContext"), which is the fail-open direction.
func TestUnpinnedInboundChainsSeesThroughQUIC(t *testing.T) {
	unpinned := &tlsv3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext:         &tlsv3.CommonTlsContext{},
	}
	l := &listenerv3.Listener{
		Name: "inbound_probe_h3",
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Protocol: corev3.SocketAddress_UDP, Address: "0.0.0.0", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 18008},
		}}},
		FilterChains: []*listenerv3.FilterChain{{
			Name: "in_h3_probe",
			TransportSocket: &corev3.TransportSocket{
				Name:       "envoy.transport_sockets.quic",
				ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: mustAny(&quicv3.QuicDownstreamTransport{DownstreamTlsContext: unpinned})},
			},
		}},
	}
	data, err := marshalBootstrap(newBootstrap([]*clusterv3.Cluster{xdsCluster()}, []*listenerv3.Listener{l}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnpinnedInboundChains(data)
	if err != nil {
		t.Fatalf("UnpinnedInboundChains: %v", err)
	}
	if len(got) != 1 || got[0] != "inbound_probe_h3/in_h3_probe" {
		t.Fatalf("an unpinned mTLS QUIC chain was not reported (got %v): the QUIC unwrap is vacuous", got)
	}
	// And the R4 check sees the same chain, for the same reason (nothing set).
	noR4, err := QUICChainsWithoutR4(data)
	if err != nil {
		t.Fatalf("QUICChainsWithoutR4: %v", err)
	}
	if len(noR4) != 1 {
		t.Fatalf("a QUIC chain with resumption/early-data UNSET was not reported by the R4 check (got %v)", noR4)
	}
}

// TestQUICUpstreamSNIIsAHostname: every `quic:` cluster's SNI must be
// "<port>.<authority>" (proxy.QUICServerName), because the QUIC client's
// hostname check runs after the SAN pin and a bare-port SNI matches no DNS SAN
// (aether#957). Runs over the generated fixture bytes.
func TestQUICUpstreamSNIIsAHostname(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bad, err := QUICUpstreamsWithPortSNI(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("quic: clusters whose SNI is not <port>.<authority> under the mesh domain: %v", bad)
	}
	// Anti-vacuity: the fixture must carry QUIC upstreams for this to test anything.
	if n := len(QUICOutboundArms()) - 1; n < 2 {
		t.Fatalf("fixture carries %d quic: twins, want >= 2", n)
	}
}

// TestQUICUpstreamsDoNotPoolPerDownstreamConnection: no `quic:` twin may set
// connection_pool_per_downstream_connection (aether#1021). Over the generated
// fixture bytes; the anti-vacuity half turns it on for every twin and requires
// each one to be flagged.
func TestQUICUpstreamsDoNotPoolPerDownstreamConnection(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bad, err := QUICUpstreamsPoolingPerDownstream(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("quic: clusters pooling per downstream connection (one QUIC connection per app connection, aether#1021): %v", bad)
	}

	bs := &bootstrapv3.Bootstrap{}
	if err := protojson.Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	var twins int
	for _, c := range bs.GetStaticResources().GetClusters() {
		if strings.HasPrefix(c.GetName(), "quic:") {
			twins++
			c.ConnectionPoolPerDownstreamConnection = true
		}
	}
	if twins < 2 {
		t.Fatalf("fixture carries %d quic: twins, want >= 2", twins)
	}
	flipped, err := protojson.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal rewritten bootstrap: %v", err)
	}
	flagged, err := QUICUpstreamsPoolingPerDownstream(flipped)
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged) != twins {
		t.Errorf("per-downstream pooling was not reported for every twin: flagged %v of %d", flagged, twins)
	}
}

// TestQUICUpstreamsIdleOutBeforeAHotRestartParentExits (aether#1054): every
// `quic:` twin carries the h3 idle timeout (config.DefaultQUICTwinIdleTimeout,
// 8s) so an idle source h3 connection is closed before the destination's
// hot-restart parent exits and its packets start drawing stateless resets;
// every other cluster that carries HTTP protocol options keeps the 30s
// config.UpstreamIdleTimeout. Over the generated fixture bytes that Envoy
// validates, so the value checked is the value Envoy accepted.
func TestQUICUpstreamsIdleOutBeforeAHotRestartParentExits(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{}
	if err := protojson.Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	var twins, others int
	for _, c := range bs.GetStaticResources().GetClusters() {
		raw, ok := c.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
		if !ok {
			continue
		}
		po := &httpv3.HttpProtocolOptions{}
		if err := raw.UnmarshalTo(po); err != nil {
			t.Fatalf("%s: unmarshal protocol options: %v", c.GetName(), err)
		}
		idle := po.GetCommonHttpProtocolOptions().GetIdleTimeout().AsDuration()
		if strings.HasPrefix(c.GetName(), "quic:") {
			twins++
			if idle != config.DefaultQUICTwinIdleTimeout {
				t.Errorf("%s: h3 twin idle timeout = %v, want %v", c.GetName(), idle, config.DefaultQUICTwinIdleTimeout)
			}
			continue
		}
		others++
		if idle != config.UpstreamIdleTimeout {
			t.Errorf("%s: idle timeout = %v, want %v (only quic: twins are shortened)", c.GetName(), idle, config.UpstreamIdleTimeout)
		}
	}
	if twins < 2 {
		t.Fatalf("fixture carries %d quic: twins, want >= 2", twins)
	}
	if others < 1 {
		t.Fatalf("fixture carries no non-twin cluster with HTTP protocol options: the 30s half is vacuous")
	}
}

// TestQUICUpstreamsDetectADeadPeer (aether#1087): every `quic:` twin carries
// QUIC-level liveness -- a keepalive PING at most every
// QUICTwinKeepaliveInterval while a request stream is open, and a transport
// idle_network_timeout of QUICTwinNetworkIdleTimeout -- so a request whose
// destination's network vanished after it was delivered (ACKed, then nothing:
// QUIC has no RST) fails within a few seconds instead of hanging to the 15 s
// route timeout. h2 clusters carry no QUIC options at all. And the route the
// twins are selected on still retries only conditions that fail before a
// request can reach an application: a liveness close resets a stream whose
// request was already sent, and that must surface as a fast 503, never as a
// replay. Over the generated fixture bytes Envoy validates.
func TestQUICUpstreamsDetectADeadPeer(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{}
	if err := protojson.Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	wantKeepalive, wantIdle := config.QUICTwinKeepaliveInterval, config.QUICTwinNetworkIdleTimeout
	var twins, others int
	for _, c := range bs.GetStaticResources().GetClusters() {
		raw, ok := c.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
		if !ok {
			continue
		}
		po := &httpv3.HttpProtocolOptions{}
		if err := raw.UnmarshalTo(po); err != nil {
			t.Fatalf("%s: unmarshal protocol options: %v", c.GetName(), err)
		}
		q := po.GetExplicitHttpConfig().GetHttp3ProtocolOptions().GetQuicProtocolOptions()
		if !strings.HasPrefix(c.GetName(), "quic:") {
			others++
			if q != nil {
				t.Errorf("%s: a non-twin cluster carries QUIC protocol options %v", c.GetName(), q)
			}
			continue
		}
		twins++
		if q == nil {
			t.Errorf("%s: no quic_protocol_options: the twin has no dead-peer detection (QUICHE idle default, 15 s keepalive), so a request to a vanished destination hangs to the route timeout", c.GetName())
			continue
		}
		if got := q.GetIdleNetworkTimeout().AsDuration(); got != wantIdle {
			t.Errorf("%s: idle_network_timeout = %v, want %v", c.GetName(), got, wantIdle)
		}
		if got := q.GetConnectionKeepalive().GetMaxInterval().AsDuration(); got != wantKeepalive {
			t.Errorf("%s: connection_keepalive.max_interval = %v, want %v", c.GetName(), got, wantKeepalive)
		}
	}
	if twins < 2 {
		t.Fatalf("fixture carries %d quic: twins, want >= 2", twins)
	}
	if others < 1 {
		t.Fatalf("fixture carries no non-twin cluster with HTTP protocol options: the h2 half is vacuous")
	}

	// The bound the PR states: the first PING leaves within keepalive + 1 s
	// (QUICHE arms the keep-alive alarm with 1 s granularity) of the peer's
	// last packet, and the idle deadline is idle_network_timeout after it.
	if bound := wantKeepalive + time.Second + wantIdle; bound > 10*time.Second {
		t.Errorf("dead-peer bound %v exceeds 10 s", bound)
	}
	// aether#1093: never at or below the 5-7 s worker stalls seen on the reference cluster.
	if wantIdle < 8*time.Second {
		t.Errorf("idle_network_timeout %v is inside the #1093 stall range", wantIdle)
	}

	// The route the twins are selected on: only pre-request conditions.
	// retriable-headers (the destination's outcome header, aether#1641) and
	// retriable-status-codes (the edge's cleartext backends) act on a RESPONSE,
	// never on a reset.
	safe := map[string]bool{"connect-failure": true, "refused-stream": true, "reset-before-request": true, "retriable-status-codes": true, "retriable-headers": true}
	var routes int
	for _, l := range bs.GetStaticResources().GetListeners() {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				hcm := &http_connection_managerv3.HttpConnectionManager{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(hcm) != nil {
					continue
				}
				for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
					for _, r := range vh.GetRoutes() {
						if _, _, ok := proxy.QUICSelectionArms(r); !ok {
							continue
						}
						routes++
						rp := r.GetRoute().GetRetryPolicy()
						if rp == nil {
							t.Errorf("the twin-selecting route has no retry policy: connect failures to a vanished endpoint would not move to a live one")
							continue
						}
						for _, cond := range strings.Split(rp.GetRetryOn(), ",") {
							if !safe[strings.TrimSpace(cond)] {
								t.Errorf("the twin-selecting route retries on %q: a request that may have reached the application could be replayed", cond)
							}
						}
						if rp.GetPerTryTimeout() != nil || rp.GetPerTryIdleTimeout() != nil {
							t.Errorf("the twin-selecting route sets a per-try timeout: that would cap slow-but-alive requests on the shared h2/h3 route")
						}
					}
				}
			}
		}
	}
	if routes != 1 {
		t.Fatalf("%d routes carry the QUIC selection plugin, want 1", routes)
	}
}

// TestH2MeshClustersDetectADeadPeer (aether#1104): every h2 mesh cluster in
// every generated bootstrap -- the node proxy's plain, per-source and
// waypointed clusters, the capture fixtures', the edge's and the QUIC
// fixture's h2 base and alias -- carries the HTTP/2 PING keepalive
// (config.MeshH2Keepalive), so a request already delivered to an endpoint
// whose pod network then vanished (no FIN, no RST: the node-shared
// destination proxy outlives the pod netns its sockets live in) fails within
// ~9 s instead of hanging to the 15 s route timeout. And every inline route
// onto one of them retries only conditions that fail before a request can
// reach an application: the keepalive close resets a stream whose request was
// already sent, and that must surface as a fast 503, never as a replay. Over
// the fixture bytes `envoy --mode validate` accepts (TestEnvoyValidate), so
// Envoy is also proven to accept the keepalive on every one of these shapes.
func TestH2MeshClustersDetectADeadPeer(t *testing.T) {
	builders := []struct {
		name string
		fn   func() ([]byte, error)
		// minimum h2 mesh clusters the fixture must carry, so the check
		// cannot pass vacuously on the bootstraps that matter.
		min int
	}{
		{"node", NodeBootstrapJSON, 3},
		{"node_cleartext", NodeCleartextBootstrapJSON, 0},
		{"node_uds", NodeUDSBootstrapJSON, 0},
		{"capture", CaptureBootstrapJSON, 0},
		{"capture_route_target", CaptureRouteTargetBootstrapJSON, 0},
		{"outbound_zero_vhost_route", OutboundZeroVhostRouteBootstrapJSON, 0},
		{"capture_tcproute", CaptureTCPRouteBootstrapJSON, 0},
		{"capture_tlsroute", CaptureTLSRouteBootstrapJSON, 0},
		{"capture_udp", CaptureUDPBootstrapJSON, 0},
		{"edge", EdgeBootstrapJSON, 1},
		{"quic_outbound", QUICOutboundBootstrapJSON, 2},
	}
	// retriable-headers (the destination's outcome header, aether#1641) and
	// retriable-status-codes (the edge's cleartext backends) act on a RESPONSE,
	// never on a reset.
	safe := map[string]bool{"connect-failure": true, "refused-stream": true, "reset-before-request": true, "retriable-status-codes": true, "retriable-headers": true}
	var total, routes int
	for _, b := range builders {
		data, err := b.fn()
		if err != nil {
			t.Fatalf("build %s: %v", b.name, err)
		}
		mesh, err := H2MeshClusters(data)
		if err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		if len(mesh) < b.min {
			t.Errorf("%s: %d h2 mesh clusters, want >= %d (the check would be vacuous)", b.name, len(mesh), b.min)
		}
		total += len(mesh)
		for name, problem := range mesh {
			if problem != "" {
				t.Errorf("%s: %s: %s", b.name, name, problem)
			}
		}

		bs := &bootstrapv3.Bootstrap{}
		if err := protojson.Unmarshal(data, bs); err != nil {
			t.Fatalf("%s: unmarshal bootstrap: %v", b.name, err)
		}
		for _, l := range bs.GetStaticResources().GetListeners() {
			for _, fc := range l.GetFilterChains() {
				for _, f := range fc.GetFilters() {
					hcm := &http_connection_managerv3.HttpConnectionManager{}
					if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(hcm) != nil {
						continue
					}
					for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
						for _, r := range vh.GetRoutes() {
							ra := r.GetRoute()
							targets := []string{ra.GetCluster()}
							for _, wc := range ra.GetWeightedClusters().GetClusters() {
								targets = append(targets, wc.GetName())
							}
							if _, base, ok := proxy.QUICSelectionArms(r); ok {
								targets = append(targets, base)
							}
							onMesh := false
							for _, c := range targets {
								if _, ok := mesh[c]; ok {
									onMesh = true
								}
							}
							if !onMesh {
								continue
							}
							routes++
							rp := ra.GetRetryPolicy()
							if rp == nil {
								t.Errorf("%s: route %q onto an h2 mesh cluster has no retry policy", b.name, r.GetName())
								continue
							}
							for _, cond := range strings.Split(rp.GetRetryOn(), ",") {
								if !safe[strings.TrimSpace(cond)] {
									t.Errorf("%s: route %q retries on %q: a request the keepalive close cut off may have reached the application and would be replayed", b.name, r.GetName(), cond)
								}
							}
							if rp.GetPerTryTimeout() != nil || rp.GetPerTryIdleTimeout() != nil {
								t.Errorf("%s: route %q sets a per-try timeout: that would cap slow-but-alive requests", b.name, r.GetName())
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d h2 mesh clusters, %d inline routes onto them", total, routes)
	if routes < 1 {
		t.Fatal("no inline route onto an h2 mesh cluster: the no-replay half is vacuous")
	}
}

// TestQUICUpstreamsHaveTheirOwnStatsKey: no `quic:` twin may report into another
// cluster's stats tree (aether#960). Over the generated fixture bytes; the
// fixture's twins are clones of the h2 cluster, which is exactly the shape
// that used to share the key.
func TestQUICUpstreamsHaveTheirOwnStatsKey(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bad, err := QUICUpstreamsSharingStatsKey(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("quic: clusters sharing a stats key with another cluster: %v", bad)
	}
}

// TestQUICUpstreamsHaveTheirOwnEDSName: no `quic:` twin may subscribe to
// another cluster's EDS resource (aether#1008). Over the generated fixture
// bytes; the fixture carries the h2 base on the bare-service EDS name, which
// is exactly the name the twins used to share. The anti-vacuity half checks
// the helper DOES flag that shape.
func TestQUICUpstreamsHaveTheirOwnEDSName(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bad, err := QUICUpstreamsSharingEDSName(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("quic: clusters sharing an EDS resource name with another cluster: %v", bad)
	}

	// Anti-vacuity: rewrite every twin back to the base's EDS name (the
	// pre-#1008 shape) and require the helper to report each one.
	bs := &bootstrapv3.Bootstrap{}
	if err := protojson.Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	var twins int
	for _, c := range bs.GetStaticResources().GetClusters() {
		if strings.HasPrefix(c.GetName(), "quic:") {
			twins++
			if c.GetEdsClusterConfig() == nil {
				t.Fatalf("twin %s has no eds_cluster_config", c.GetName())
			}
			c.EdsClusterConfig.ServiceName = quicDestSvc
		}
	}
	if twins < 2 {
		t.Fatalf("fixture carries %d quic: twins, want >= 2", twins)
	}
	shared, err := protojson.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal rewritten bootstrap: %v", err)
	}
	flagged, err := QUICUpstreamsSharingEDSName(shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged) != twins {
		t.Errorf("the shared-EDS-name shape was not reported for every twin: flagged %v of %d", flagged, twins)
	}
}

// TestNoNonDefaultClusterSharesTheServiceEDSName is the aether#1013 gate over
// the fixtures that carry the cluster kinds that used to share the default
// cluster's bare-service EDS name: the port alias (quic_outbound, next to the
// default cluster and the twins) and the TCP floors (capture_tcproute,
// capture_tlsroute). The fixtures must pass ClustersSharingServiceEDSName; the
// anti-vacuity half rewrites every alias, floor and twin back to the bare
// service name (its alt_stat_name, which is the bare key for all three --
// with the twin's "@<ns>/<sa>" suffix stripped) and requires each one to be
// flagged.
func TestNoNonDefaultClusterSharesTheServiceEDSName(t *testing.T) {
	for _, b := range []struct {
		name string
		fn   func() ([]byte, error)
		// kinds is the minimum count of each rewritten kind the fixture must
		// carry, so the check is not vacuous.
		kinds map[string]int
	}{
		{"quic_outbound_bootstrap.json", QUICOutboundBootstrapJSON, map[string]int{"alias": 1, "quic": 2}},
		{"capture_tcproute_bootstrap.json", CaptureTCPRouteBootstrapJSON, map[string]int{"tcp": 2}},
		{"capture_tlsroute_bootstrap.json", CaptureTLSRouteBootstrapJSON, map[string]int{"tcp": 3}},
	} {
		t.Run(b.name, func(t *testing.T) {
			data, err := b.fn()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			bad, err := ClustersSharingServiceEDSName(data)
			if err != nil {
				t.Fatal(err)
			}
			if len(bad) > 0 {
				t.Errorf("clusters not subscribed to an EDS name of their own: %v", bad)
			}

			bs := &bootstrapv3.Bootstrap{}
			if err := protojson.Unmarshal(data, bs); err != nil {
				t.Fatalf("unmarshal bootstrap: %v", err)
			}
			seen := map[string]int{}
			var rewritten []string
			for _, c := range bs.GetStaticResources().GetClusters() {
				var kind string
				switch {
				case strings.HasPrefix(c.GetName(), "quic:"):
					kind = "quic"
				case strings.HasPrefix(c.GetName(), "tcp:"):
					kind = "tcp"
				case c.GetType() == clusterv3.Cluster_EDS && strings.Contains(c.GetName(), ":"):
					kind = "alias"
				default:
					continue
				}
				bare, _, _ := strings.Cut(c.GetAltStatName(), "@")
				// An L4 cluster's stat key carries its kind (aether#1023):
				// tcp_<ns>/<svc>[_<port>]. The bare service key is between.
				if rest, ok := strings.CutPrefix(bare, proxy.L4StatKeyTCPPrefix); ok {
					bare, _, _ = strings.Cut(rest, "_")
				}
				if bare == "" || c.GetEdsClusterConfig() == nil {
					t.Fatalf("%s %s has no bare alt_stat_name or no eds_cluster_config", kind, c.GetName())
				}
				seen[kind]++
				c.EdsClusterConfig.ServiceName = bare
				rewritten = append(rewritten, c.GetName())
			}
			for kind, want := range b.kinds {
				if seen[kind] < want {
					t.Fatalf("fixture carries %d %s clusters, want >= %d: the check would be vacuous", seen[kind], kind, want)
				}
			}
			shared, err := protojson.Marshal(bs)
			if err != nil {
				t.Fatalf("marshal rewritten bootstrap: %v", err)
			}
			flagged, err := ClustersSharingServiceEDSName(shared)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range rewritten {
				if !slices.Contains(flagged, name) {
					t.Errorf("the bare-service-EDS shape of %s was not reported (flagged %v)", name, flagged)
				}
			}
		})
	}
}

// TestQUICOutboundFixtureCarriesTheSelection is the anti-vacuity half of the
// QUIC upstream checks: the fixture must contain both `quic:` twins with a
// QuicUpstreamTransport, and a route whose matcher arms map each source
// identity to its own twin with the h2 cluster as on_no_match -- otherwise
// the validate run and the R4/pin checks above are exercising nothing.
func TestQUICOutboundFixtureCarriesTheSelection(t *testing.T) {
	data, err := QUICOutboundBootstrapJSON()
	if err != nil {
		t.Fatalf("QUICOutboundBootstrapJSON: %v", err)
	}
	bs := &bootstrapv3.Bootstrap{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, bs); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	want := QUICOutboundArms()
	h2 := want[""]
	delete(want, "")
	quicClusters := map[string]bool{}
	for _, c := range bs.GetStaticResources().GetClusters() {
		if c.GetTransportSocket().GetTypedConfig() != nil && c.GetTransportSocket().GetTypedConfig().MessageIs(&quicv3.QuicUpstreamTransport{}) {
			quicClusters[c.GetName()] = true
		}
	}
	for id, name := range want {
		if !quicClusters[name] {
			t.Errorf("source %s: twin %q is not a QUIC-transport cluster in the fixture", id, name)
		}
	}
	var routes int
	for _, l := range bs.GetStaticResources().GetListeners() {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				hcm := &http_connection_managerv3.HttpConnectionManager{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(hcm) != nil {
					continue
				}
				for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
					for _, r := range vh.GetRoutes() {
						arms, noMatch, ok := proxy.QUICSelectionArms(r)
						if !ok {
							continue
						}
						routes++
						if noMatch != h2 {
							t.Errorf("on_no_match = %q, want the h2 cluster %q", noMatch, h2)
						}
						for id, name := range want {
							if arms[id] != name {
								t.Errorf("arm %s = %q, want %q", id, arms[id], name)
							}
						}
						// Demand-scoped twins (aether#1020): an unobserved
						// source keeps its arm, and its twin is NOT built.
						for id, name := range QUICOutboundUnobservedArms() {
							if arms[id] != name {
								t.Errorf("unobserved arm %s = %q, want %q", id, arms[id], name)
							}
							if quicClusters[name] {
								t.Errorf("twin %q for an unobserved pair is built; it must be fetched on demand", name)
							}
						}
						if n := len(hcm.GetHttpFilters()); n < 2 || hcm.GetHttpFilters()[0].GetName() != "envoy.filters.http.on_demand" {
							t.Errorf("the selecting HCM must run the on_demand filter first (an arm to an unbuilt twin 503s without it): %v", hcm.GetHttpFilters())
						}
						if r.GetRoute().GetEarlyDataPolicy() != nil {
							t.Errorf("route to a quic: cluster sets early_data_policy (038 R4)")
						}
					}
				}
			}
		}
	}
	if routes != 1 {
		t.Fatalf("%d routes carry the QUIC selection plugin, want 1: the validate run is not exercising it", routes)
	}
}

// TestUnpinnedMeshClustersSeesThroughQUIC proves the QUIC unwrap in
// upstreamTLSContextOf is load-bearing: a `quic:` cluster whose inner context
// pins nothing must be reported (before the unwrap it passed vacuously).
func TestUnpinnedMeshClustersSeesThroughQUIC(t *testing.T) {
	c := &clusterv3.Cluster{
		Name:                 "quic:probe",
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		TransportSocket: &corev3.TransportSocket{
			Name: "envoy.transport_sockets.quic",
			ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: mustAny(&quicv3.QuicUpstreamTransport{
				UpstreamTlsContext: &tlsv3.UpstreamTlsContext{CommonTlsContext: &tlsv3.CommonTlsContext{}},
			})},
		},
	}
	data, err := marshalBootstrap(newBootstrap([]*clusterv3.Cluster{xdsCluster(), c}, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnpinnedMeshClusters(data)
	if err != nil {
		t.Fatalf("UnpinnedMeshClusters: %v", err)
	}
	if len(got) != 1 || got[0] != "quic:probe" {
		t.Fatalf("an unpinned QUIC upstream was not reported (got %v): the QUIC unwrap is vacuous", got)
	}
	cached, err := QUICUpstreamsWithSessionCache(data)
	if err != nil {
		t.Fatalf("QUICUpstreamsWithSessionCache: %v", err)
	}
	if len(cached) != 1 {
		t.Fatalf("a QUIC upstream with max_session_keys UNSET was not reported (got %v)", cached)
	}
}
