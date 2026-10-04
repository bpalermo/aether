package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
)

// tgz builds a gzipped tar of name -> content entries, in order.
func tgz(t *testing.T, entries ...[2]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o644, Size: int64(len(e[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

const chartYAML = `apiVersion: v2
name: aether
description: Aether service mesh
type: application
version: "1.0.0-0123456789abcdef0123456789abcdef01234567"
appVersion: "0.1.0"
keywords:
  - mesh
`

func TestReadConvertsTheTopLevelChartYAML(t *testing.T) {
	c, err := Read(tgz(t,
		[2]string{"aether/values.yaml", "a: 1\n"},
		// A dependency's Chart.yaml is deeper and must never be picked.
		[2]string{"aether/charts/dep/Chart.yaml", "apiVersion: v2\nname: dep\nversion: 9.9.9\n"},
		[2]string{"aether/Chart.yaml", chartYAML},
	))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "aether" || c.Version != "1.0.0-0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("name/version = %q/%q", c.Name, c.Version)
	}
	var cfg map[string]any
	if err := json.Unmarshal(c.Config, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	// The keys helm's chart.Metadata marshals to are Chart.yaml's own.
	for k, want := range map[string]any{
		"apiVersion":  "v2",
		"name":        "aether",
		"description": "Aether service mesh",
		"type":        "application",
		"appVersion":  "0.1.0",
	} {
		if cfg[k] != want {
			t.Errorf("config[%q] = %v, want %v", k, cfg[k], want)
		}
	}
	if kw, ok := cfg["keywords"].([]any); !ok || len(kw) != 1 || kw[0] != "mesh" {
		t.Errorf("config[keywords] = %v", cfg["keywords"])
	}
}

func TestReadRefusesWhatIsNotOnePackagedChart(t *testing.T) {
	for name, archive := range map[string]*bytes.Reader{
		"no Chart.yaml":               tgz(t, [2]string{"aether/values.yaml", "a: 1\n"}),
		"Chart.yaml only in a dep":    tgz(t, [2]string{"aether/charts/dep/Chart.yaml", chartYAML}),
		"Chart.yaml at the root":      tgz(t, [2]string{"Chart.yaml", chartYAML}),
		"two top-level charts":        tgz(t, [2]string{"a/Chart.yaml", chartYAML}, [2]string{"b/Chart.yaml", chartYAML}),
		"no version":                  tgz(t, [2]string{"aether/Chart.yaml", "apiVersion: v2\nname: aether\n"}),
		"no name":                     tgz(t, [2]string{"aether/Chart.yaml", "apiVersion: v2\nversion: 1.0.0\n"}),
		"not YAML":                    tgz(t, [2]string{"aether/Chart.yaml", "name: [\n"}),
		"not a mapping":               tgz(t, [2]string{"aether/Chart.yaml", "- a\n- b\n"}),
		"not a gzip stream (a plain)": bytes.NewReader([]byte("plain text")),
	} {
		t.Run(name, func(t *testing.T) {
			if c, err := Read(archive); err == nil {
				t.Fatalf("accepted: %+v", c)
			}
		})
	}
}

func TestParseKeepsAnUnstampedVersionForThePushToRefuse(t *testing.T) {
	c, err := Parse([]byte("apiVersion: v2\nname: crds\nversion: \"1.0.0-{GIT_COMMIT}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Version, "{GIT_COMMIT}") {
		t.Fatalf("version = %q", c.Version)
	}
}
