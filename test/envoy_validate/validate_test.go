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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		{"capture_tcproute_bootstrap.json", CaptureTCPRouteBootstrapJSON},
		{"capture_tlsroute_bootstrap.json", CaptureTLSRouteBootstrapJSON},
		{"capture_udp_bootstrap.json", CaptureUDPBootstrapJSON},
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

// chainByName returns a listener's filter chain with the given name.
func chainByName(l *listenerv3.Listener, name string) *listenerv3.FilterChain {
	for _, fc := range l.GetFilterChains() {
		if fc.GetName() == name {
			return fc
		}
	}
	return nil
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
