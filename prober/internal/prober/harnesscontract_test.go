package prober

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/test/harnesscontract"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

// The tier and result vocabularies are the string constants of prober.go named
// tier* and result*. They are read from the source, not listed here a second
// time: a new result is a new value in a closed set a harness outside this
// repository branches on, and a list in this test would be the one place it
// was forgotten.
//
//go:embed prober.go
var proberSource string

// stringConstants returns the values of the package-level string constants of
// prober.go whose name starts with prefix.
func stringConstants(t *testing.T, prefix string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "prober.go", proberSource, 0)
	if err != nil {
		t.Fatalf("parse prober.go: %v", err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("constant %s is not a string literal: this test reads the %s* vocabulary from literals", name.Name, prefix)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("constant %s: %v", name.Name, err)
				}
				out = append(out, value)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("prober.go has no string constant named %s*: the vocabulary moved, and this test must follow it", prefix)
	}
	return out
}

const contractTarget = "//prober/internal/prober:prober_test"

// TestExternalHarnessContract_Metric holds aether_probe_requests_total to the
// external-harness contract (test/harnesscontract/external-harness.yaml): its
// name, its labels, and the closed tier and result sets. Every (tier, result)
// the constants allow goes through the real record(), and what the instrument
// then holds is compared with the contract.
func TestExternalHarnessContract_Metric(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	c.Owns(t, contractTarget, "prober.probe_requests", "resource.service_name.prober",
		"prober.probe_fail.detail", "prober.probe_fail.summary")
	entry := c.Metric(t, "prober.probe_requests")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	res := resource.NewSchemaless(semconv.K8SPodName(testPod), semconv.K8SNodeName(testNode))
	p, err := newProber(DefaultConfig(), slog.New(slog.DiscardHandler), res, provider.Meter("test"), nil, &syncBuffer{})
	if err != nil {
		t.Fatalf("newProber: %v", err)
	}
	for _, tier := range stringConstants(t, "tier") {
		for _, result := range stringConstants(t, "result") {
			p.record(target{tier: tier, name: "a-target"}, result, 0.001, nil, noPhase)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	kind, series := "", []harnesscontract.Series(nil)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != entry.OTelName {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				kind = "something other than a monotonic sum"
				continue
			}
			kind = harnesscontract.TypeCounter
			for _, dp := range sum.DataPoints {
				s := harnesscontract.Series{}
				for _, kv := range dp.Attributes.ToSlice() {
					s[string(kv.Key)] = kv.Value.Emit()
				}
				series = append(series, s)
			}
		}
	}
	entry.CheckMetric(t, kind, series)
}

// TestExternalHarnessContract_ServiceName: the resource the prober exports
// under, which a pipeline commonly turns into the `job` label.
func TestExternalHarnessContract_ServiceName(t *testing.T) {
	entry := harnesscontract.MustLoad(t).ResourceAttribute(t, "resource.service_name.prober")
	res, err := newResource(context.Background(), "test")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if entry.Attribute != string(semconv.ServiceNameKey) {
		harnesscontract.Errorf(t, "%s: %s names the attribute %q; the prober sets %q.", harnesscontract.File, entry.ID, entry.Attribute, semconv.ServiceNameKey)
	}
	if got := resourceString(res, semconv.ServiceNameKey); got != entry.Value {
		harnesscontract.Errorf(t, "%s says the prober's %s is %q; its resource says %q.", harnesscontract.File, entry.Attribute, entry.Value, got)
	}
}

// keysInOrder returns the keys of one marker-prefixed JSON line in the order
// they were written, and the line decoded.
func keysInOrder(t *testing.T, marker, line string) ([]string, map[string]any) {
	t.Helper()
	raw, ok := strings.CutPrefix(line, marker)
	if !ok {
		harnesscontract.Errorf(t, "%s says the line starts with the marker %q; the prober wrote %q.", harnesscontract.File, marker, line)
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("the line after the marker is not one JSON object: %q", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read a key of %q: %v", raw, err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("read the value of %q in %q: %v", keys[len(keys)-1], raw, err)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return keys, decoded
}

// TestExternalHarnessContract_FailLines holds the AETHER_PROBE_FAIL lines to
// the contract. A burst goes through the real fail log, built with the real
// cap and window, and a stopping prober's flush closes it; the lines it wrote
// are then read the way a harness reads them: the marker, one JSON object, the
// contract's field list, and the times as RFC 3339.
func TestExternalHarnessContract_FailLines(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	detail := c.LogLine(t, "prober.probe_fail.detail")
	summary := c.LogLine(t, "prober.probe_fail.summary")

	if detail.CapPerWindow != failLogCap {
		harnesscontract.Errorf(t, "%s says at most %d detail lines per (tier, result) per window; the prober's cap is %d.", harnesscontract.File, detail.CapPerWindow, failLogCap)
	}
	for _, entry := range []harnesscontract.LogLine{detail, summary} {
		if got := int(failLogWindow.Seconds()); entry.WindowSeconds != got {
			harnesscontract.Errorf(t, "%s says the window of %s is %d s; the prober's is %d s.", harnesscontract.File, entry.ID, entry.WindowSeconds, got)
		}
	}

	var out bytes.Buffer
	f := newFailLog(&out, testPod, testNode, failLogCap, failLogWindow)
	opened := time.Date(2026, 10, 8, 4, 38, 0, 500_000_000, time.UTC)
	const past = 3 // failures past the cap
	tgt := target{tier: tierMeshDNS, name: "echo.a-namespace.aether.internal:18081"}
	for i := range failLogCap + past {
		f.log(opened.Add(time.Duration(i)*time.Millisecond), tgt, resultTimeout, 2, context.DeadlineExceeded, noPhase)
	}
	stopped := opened.Add(10 * time.Second) // the prober stops: the window is ten seconds old
	f.flushAll(stopped)

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != failLogCap+1 {
		t.Fatalf("the fail log wrote %d lines for %d failures and a stop, want %d detail lines and one summary", len(lines), failLogCap+past, failLogCap)
	}

	// The detail lines.
	for i, line := range lines[:failLogCap] {
		keys, decoded := keysInOrder(t, detail.Marker, line)
		detail.CheckFields(t, keys)
		checkTimes(t, detail, decoded)
		if _, isSummary := decoded["suppressed"]; isSummary {
			harnesscontract.Errorf(t, "%s says a detail line never has the key `suppressed` (it is what tells a summary apart); line %d has it.", harnesscontract.File, i+1)
		}
		if last := i == failLogCap-1; decoded["truncated"] != last {
			harnesscontract.Errorf(t, "%s says the line that reaches the cap, and only that one, has truncated=true; line %d of %d has truncated=%v.", harnesscontract.File, i+1, failLogCap, decoded["truncated"])
		}
		if t.Failed() {
			return // one line is enough to say what differs
		}
	}

	// The summary a stopping prober writes.
	keys, decoded := keysInOrder(t, summary.Marker, lines[failLogCap])
	summary.CheckFields(t, keys)
	checkTimes(t, summary, decoded)
	for key, want := range map[string]any{
		"suppressed":   float64(past),
		"window_s":     failLogWindow.Seconds(),
		"window_start": opened.Format(time.RFC3339Nano),
		// Stamped with the time the prober stopped, not a window later (#1463).
		"t":      stopped.Format(time.RFC3339Nano),
		"tier":   tierMeshDNS,
		"result": resultTimeout,
	} {
		if decoded[key] != want {
			harnesscontract.Errorf(t, "the summary of a stopping prober has %s=%v, and %s describes %v (see the notes of %s).", key, decoded[key], harnesscontract.File, want, summary.ID)
		}
	}
}

// checkTimes parses the entry's time fields the way a harness does.
func checkTimes(t *testing.T, entry harnesscontract.LogLine, decoded map[string]any) {
	t.Helper()
	for _, field := range entry.TimeFields {
		text, _ := decoded[field].(string)
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || parsed.Location() != time.UTC {
			harnesscontract.Errorf(t, "%s says %s of %s is an RFC 3339 time in UTC; the prober wrote %v.", harnesscontract.File, field, entry.ID, decoded[field])
		}
	}
}

// The vocabulary the test reads from the source is the one the code uses: a
// guard against the parse silently finding other constants.
func TestStringConstantsFindTheVocabulary(t *testing.T) {
	if got := stringConstants(t, "tier"); !slices.Contains(got, tierMeshDNS) || !slices.Contains(got, tierLiveness) {
		t.Errorf("tier constants = %v", got)
	}
	if got := stringConstants(t, "result"); !slices.Contains(got, resultSuccess) || !slices.Contains(got, resultDNSTimeout) {
		t.Errorf("result constants = %v", got)
	}
}
