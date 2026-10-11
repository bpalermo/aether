package proxy

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/test/harnesscontract"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
)

// ownsContractEntries declares the contract entries this package's tests hold
// to the code, for every test that holds one: the contract must assign exactly
// these to //agent/internal/xds/proxy:proxy_test.
func ownsContractEntries(t *testing.T, c *harnesscontract.Contract) {
	t.Helper()
	c.Owns(t, "//agent/internal/xds/proxy:proxy_test", "envoy.capture.anyport_downstream_cx", "proxy.access_log.http")
}

// TestExternalHarnessContract_AccessLog holds the HTTP access log to the
// external-harness contract: the stream's log_name, the field that names the
// reporter and its two values, and each field the contract lists with the
// Envoy format operator that renders it. A harness outside this repository
// selects records by these fields, and a query over a field that was renamed
// or dropped matches nothing: its "no such request" then reads exactly like a
// clean run. The record is the one the real builder makes, for each reporter.
func TestExternalHarnessContract_AccessLog(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	ownsContractEntries(t, c)
	entry := c.AccessLog(t, "proxy.access_log.http")

	t.Cleanup(func() { SetAccessLogConfig(AccessLogConfig{}) })
	SetAccessLogConfig(AccessLogConfig{Enabled: true, SuccessSampleRate: 100})

	code := []string{ReporterSource, ReporterDestination}
	if missing, extra := diffStrings(entry.Reporters, code); len(missing)+len(extra) > 0 {
		harnesscontract.Errorf(t, "%s lists the reporters of %s as %v; the code has %v. A value added to or removed from this closed set bumps `version`.",
			harnesscontract.File, entry.ID, entry.Reporters, code)
	}
	for _, reporter := range code {
		logs := buildAccessLog(reporter, "a-pod", "a-namespace")
		if len(logs) != 1 {
			t.Fatalf("buildAccessLog(%s) returned %d access logs, want 1", reporter, len(logs))
		}
		cfg := &otelaccesslogv3.OpenTelemetryAccessLogConfig{}
		if err := logs[0].GetTypedConfig().UnmarshalTo(cfg); err != nil {
			t.Fatalf("buildAccessLog(%s): %v", reporter, err)
		}
		if cfg.GetLogName() != entry.LogName {
			harnesscontract.Errorf(t, "%s says the log_name of %s is %q; the %s access log is exported as %q.",
				harnesscontract.File, entry.ID, entry.LogName, reporter, cfg.GetLogName())
		}
		attrs := map[string]string{}
		for _, kv := range cfg.GetAttributes().GetValues() {
			if _, twice := attrs[kv.GetKey()]; twice {
				t.Errorf("the %s access log has the attribute %q twice: which one a store keeps is not decidable", reporter, kv.GetKey())
			}
			attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		if got, ok := attrs[entry.ReporterField]; !ok || got != reporter {
			harnesscontract.Errorf(t, "%s says the field %q of %s names the reporter; the %s access log has %q there (present: %v).",
				harnesscontract.File, entry.ReporterField, entry.ID, reporter, got, ok)
		}
		for field, format := range entry.Fields {
			got, ok := attrs[field]
			switch {
			case !ok:
				harnesscontract.Errorf(t, "%s lists the field %q of %s; the %s access log has no such field. A removed or renamed field bumps `version`.",
					harnesscontract.File, field, entry.ID, reporter)
			case got != format:
				harnesscontract.Errorf(t, "%s says the field %q of %s is rendered by %q; the %s access log renders it with %q. A field that means something else bumps `version`.",
					harnesscontract.File, field, entry.ID, format, reporter, got)
			}
		}
	}
}

// diffStrings returns what only want has and what only got has.
func diffStrings(want, got []string) (onlyWant, onlyGot []string) {
	for _, v := range want {
		if !slices.Contains(got, v) {
			onlyWant = append(onlyWant, v)
		}
	}
	for _, v := range got {
		if !slices.Contains(want, v) {
			onlyGot = append(onlyGot, v)
		}
	}
	return onlyWant, onlyGot
}

// TestExternalHarnessContract_AnyPortStat holds the any-port shim's stat
// prefix to the external-harness contract
// (test/harnesscontract/external-harness.yaml). A harness outside this
// repository reads tcp.<prefix><cluster>.downstream_cx_total by pattern, and a
// counter Envoy never incremented has no series at all: after a rename its
// query would read "no data", which is exactly what a legitimate zero looks
// like. The prefix is taken from a capture listener the real generator built.
func TestExternalHarnessContract_AnyPortStat(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	ownsContractEntries(t, c)
	entry := c.EnvoyStat(t, "envoy.capture.anyport_downstream_cx")

	const cluster = "tcp:echo-tcp.a-namespace.aether.internal"
	l, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"},
		"spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false,
		[]CaptureTCPService{{ClusterName: cluster, ClusterIP: "10.96.1.50", LegacyTCPPort: 9000, AnyPort: true}}, nil, true, nil)
	if err != nil {
		t.Fatalf("GenerateCaptureListener: %v", err)
	}
	var portless []string // the stat prefixes of the chains with no destination_port
	for _, fc := range l.GetFilterChains() {
		if fc.GetFilterChainMatch().GetDestinationPort().GetValue() != 0 {
			continue
		}
		for _, f := range fc.GetFilters() {
			tc := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig() != nil && f.GetTypedConfig().UnmarshalTo(tc) == nil {
				portless = append(portless, tc.GetStatPrefix())
			}
		}
	}
	want := entry.StatPrefix + cluster
	found := false
	for _, p := range portless {
		found = found || p == want
	}
	if !found {
		harnesscontract.Errorf(t, "%s says the any-port chain of a TCP-primary service counts under the stat prefix %q + the cluster name (%q); the portless chains of the generated capture listener have %q.",
			harnesscontract.File, entry.StatPrefix, want, portless)
	}

	// The rest of the entry is what Envoy and a metrics pipeline make of the
	// prefix. Not something this repository can run, but the three spellings
	// in the entry must at least agree with one another.
	if wantStat := "tcp." + entry.StatPrefix + "<cluster>.downstream_cx_total"; entry.Stat != wantStat {
		harnesscontract.Errorf(t, "%s: the stat of %s is %q, and its stat_prefix makes it %q.", harnesscontract.File, entry.ID, entry.Stat, wantStat)
	}
	flat := "envoy_" + regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString("tcp."+want+".downstream_cx_total", "_")
	pattern, err := regexp.Compile("^(?:" + entry.StoredNamePattern + ")$")
	if err != nil {
		t.Fatalf("%s: stored_name_pattern of %s is not a regular expression: %v", harnesscontract.File, entry.ID, err)
	}
	if !pattern.MatchString(flat) || !strings.Contains(entry.StoredNamePattern, strings.TrimSuffix(entry.StatPrefix, "_")) {
		harnesscontract.Errorf(t, "%s: stored_name_pattern %q of %s does not match %q, the flattened name of the stat for cluster %q.", harnesscontract.File, entry.StoredNamePattern, entry.ID, flat, cluster)
	}
}
