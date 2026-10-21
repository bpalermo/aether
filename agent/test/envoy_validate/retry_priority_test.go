package envoy_validate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/config"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	previous_hostsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/host/previous_hosts/v3"
	previous_prioritiesv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/priority/previous_priorities/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// eachInlineRetryPolicy calls fn on the retry policy of every route of every
// route table a bootstrap's listeners carry inline, and on the listener and
// connection manager that hold it. Every listener whose HCM serves its table
// over RDS is skipped: there is nothing to see in the bytes.
func eachInlineRetryPolicy(t *testing.T, bs *bootstrapv3.Bootstrap, fn func(where string, rp *routev3.RetryPolicy)) {
	t.Helper()
	for _, l := range bs.GetStaticResources().GetListeners() {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				hcm := &http_connection_managerv3.HttpConnectionManager{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(hcm) != nil {
					continue
				}
				for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
					for _, r := range vh.GetRoutes() {
						if rp := r.GetRoute().GetRetryPolicy(); rp != nil {
							fn(l.GetName()+"/"+vh.GetName(), rp)
						}
					}
				}
			}
		}
	}
}

// TestEveryGeneratedRetryLeavesAttemptedPriorities: every retry policy in the
// bootstraps TestEnvoyValidate hands the pinned Envoy carries
// previous_priorities with update_frequency 1, so that the validation there
// is a validation of it (the gate is not vacuous); and the pinned Envoy
// rejects the same bytes when the retry priority names an extension it does
// not have, so that a validation which passes has resolved the
// previous_priorities factory: the extension is compiled into the proxy.
//
// What the validation does NOT check, measured on the pinned proxy: the
// value of update_frequency. Envoy resolves the factory and unpacks the
// config when it loads the route, but validates the config (update_frequency
// gt 0) only when it creates the retry priority, on a request's first retry.
// A 0 passes `--mode validate`; the unit tests in agent/internal/xds/proxy
// are what hold the value at 1.
func TestEveryGeneratedRetryLeavesAttemptedPriorities(t *testing.T) {
	envoy := envoyBinary(t)
	const notCompiledIn = "aether.test.retry_priority_not_compiled_in"
	var policies int
	builders := []struct {
		name  string
		build func() ([]byte, error)
	}{
		{"capture_perport_bootstrap.json", CapturePerPortBootstrapJSON},
		{"capture_route_target_bootstrap.json", CaptureRouteTargetBootstrapJSON},
		{"edge_bootstrap.json", EdgeBootstrapJSON},
		{"quic_outbound_bootstrap.json", QUICOutboundBootstrapJSON},
	}
	var withPolicies []byte
	for _, b := range builders {
		name := b.name
		data, err := b.build()
		if err != nil {
			t.Fatalf("build %s: %v", name, err)
		}
		bs := &bootstrapv3.Bootstrap{}
		if err := protojson.Unmarshal(data, bs); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		before := policies
		eachInlineRetryPolicy(t, bs, func(where string, rp *routev3.RetryPolicy) {
			policies++
			pr := rp.GetRetryPriority()
			if pr.GetName() != "envoy.retry_priorities.previous_priorities" {
				t.Errorf("%s %s: retry_priority = %q, want previous_priorities: a retry stays at the priority that failed", name, where, pr.GetName())
				return
			}
			cfg := &previous_prioritiesv3.PreviousPrioritiesConfig{}
			if err := pr.GetTypedConfig().UnmarshalTo(cfg); err != nil {
				t.Errorf("%s %s: retry_priority typed config: %v", name, where, err)
				return
			}
			if cfg.GetUpdateFrequency() != 1 {
				t.Errorf("%s %s: update_frequency = %d, want 1", name, where, cfg.GetUpdateFrequency())
			}
		})
		if policies > before && withPolicies == nil {
			withPolicies = data
		}
	}
	if policies == 0 || withPolicies == nil {
		t.Fatal("no bootstrap carries an inline retry policy: the validation of the retry priority would be vacuous")
	}

	validate := func(name string, data []byte) (string, error) {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		out, err := exec.Command(envoy, "--mode", "validate", "-c", path).CombinedOutput()
		return string(out), err
	}
	if out, err := validate("accepted.json", withPolicies); err != nil {
		t.Fatalf("the pinned Envoy rejects the generated retry priority: %v\n%s", err, out)
	}

	// The control: the same bytes with a retry priority this Envoy does not
	// have. The type URL is that of a registered extension of another
	// category (a retry HOST predicate), so neither the type nor the name
	// resolves to a retry priority factory.
	bs := &bootstrapv3.Bootstrap{}
	if err := protojson.Unmarshal(withPolicies, bs); err != nil {
		t.Fatal(err)
	}
	for _, l := range bs.GetStaticResources().GetListeners() {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				hcm := &http_connection_managerv3.HttpConnectionManager{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(hcm) != nil {
					continue
				}
				changed := false
				for _, vh := range hcm.GetRouteConfig().GetVirtualHosts() {
					for _, r := range vh.GetRoutes() {
						if pr := r.GetRoute().GetRetryPolicy().GetRetryPriority(); pr != nil {
							pr.Name = notCompiledIn
							pr.ConfigType = &routev3.RetryPolicy_RetryPriority_TypedConfig{
								TypedConfig: config.TypedConfig(&previous_hostsv3.PreviousHostsPredicate{}),
							}
							changed = true
						}
					}
				}
				if changed {
					f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)}
				}
			}
		}
	}
	data, err := protojson.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := validate("rejected.json", data)
	if err == nil {
		t.Fatalf("the pinned Envoy accepts a retry priority it does not have: the validation above did not resolve previous_priorities\n%s", out)
	}
	if !strings.Contains(out, notCompiledIn) {
		t.Fatalf("rejected for another reason than the unknown retry priority:\n%s", out)
	}
}
