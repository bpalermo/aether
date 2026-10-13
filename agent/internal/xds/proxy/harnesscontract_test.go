package proxy

import (
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
	c.Owns(t, "//agent/internal/xds/proxy:proxy_test", "proxy.access_log.http")
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

// TestExternalHarnessContract_AnyPortStat held the any-port shim's stat prefix
// to the external-harness contract until version 3. Version 4 removed the
// entry together with the chain (chart 3.0.0), and what the test holds now is
// the removal: the contract has no Envoy stat entry, and no chain of a capture
// listener the real generator built counts under the prefix a version-3
// harness reads. A harness reading it sees no series, and the contract's
// version history says that this is what "removed" looks like; were the
// prefix to come back without an entry, the same harness would read a counter
// nobody promised.
func TestExternalHarnessContract_AnyPortStat(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	ownsContractEntries(t, c)
	if len(c.EnvoyStats) != 0 {
		harnesscontract.Errorf(t, "%s has %d envoy_stats entries; this package holds none of them to the code. An Envoy stat a harness may read needs a test that takes its prefix from a generated listener.",
			harnesscontract.File, len(c.EnvoyStats))
	}

	const removedPrefix = "cap_tcp_anyport_"
	for name, svcs := range map[string][]CaptureTCPService{
		"raw-TCP legacy target":                          {{ClusterName: "tcp:echo-tcp.a-namespace.aether.internal", ClusterIP: "10.96.1.50", LegacyTCPPort: 9000}},
		"raw-TCP legacy target and another raw-TCP port": {{ClusterName: "tcp:echo-tcp.a-namespace.aether.internal", ClusterIP: "10.96.1.50", LegacyTCPPort: 9000, TCPPorts: []uint32{9000, 9100}}},
		"mixed, HTTP legacy target":                      {{ClusterName: "tcp:mixed.a-namespace.aether.internal", ClusterIP: "10.96.1.51", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}}},
	} {
		for _, withPassthrough := range []bool{true, false} {
			l, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"},
				"spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false,
				svcs, []uint32{80, 8080, 18081}, withPassthrough, nil)
			if err != nil {
				t.Fatalf("GenerateCaptureListener(%s): %v", name, err)
			}
			tcpProxies := 0
			for _, fc := range l.GetFilterChains() {
				if strings.Contains(fc.GetName(), removedPrefix) {
					harnesscontract.Errorf(t, "%s (version 4) says the any-port chain is gone; the capture listener of %q has the chain %q.", harnesscontract.File, name, fc.GetName())
				}
				for _, f := range fc.GetFilters() {
					tc := &tcp_proxyv3.TcpProxy{}
					if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
						continue
					}
					tcpProxies++
					if strings.Contains(tc.GetStatPrefix(), removedPrefix) {
						harnesscontract.Errorf(t, "%s (version 4) says no chain counts under %q; the chain %q of %q has the stat prefix %q.", harnesscontract.File, removedPrefix, fc.GetName(), name, tc.GetStatPrefix())
					}
				}
			}
			if tcpProxies == 0 {
				t.Fatalf("the capture listener of %q has no tcp_proxy chain: the check above looked at nothing", name)
			}
		}
	}
}
