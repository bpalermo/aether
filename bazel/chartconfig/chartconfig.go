// Package main is chartconfig: it reads a packaged Helm chart (the .tgz
// `helm package` writes) and emits the OCI config blob `helm push` would attach
// to it, plus the chart's name and version.
//
// Why it exists (proposal 040). Charts are published to quay.io, which has no
// nested repositories, under a flat `chart-<name>` repository that `helm push`
// cannot address (it always appends Chart.yaml's `name:`). chart_push in
// //bazel/helm:defs.bzl therefore writes the same OCI artifact helm would --
// the .tgz as an `application/vnd.cncf.helm.chart.content.v1.tar+gzip` layer
// and the chart metadata as an `application/vnd.cncf.helm.config.v1+json`
// config -- with `oras push`, naming the repository outright. helm builds that
// config as json.Marshal(chart.Metadata), whose JSON keys are Chart.yaml's own
// keys; converting Chart.yaml from YAML to JSON therefore yields the same
// document (up to key order and omitted empties, which no reader depends on),
// and `helm pull oci://...` reads it back unchanged.
//
// It runs as a build action on the STAMPED package, so the version it reports
// is the one the push publishes under (a `{GIT_COMMIT}` suffix already
// substituted). It does not validate the version as an OCI tag: an unstamped
// build is legal to build, and chart_push refuses to push a tag that is not
// one.
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"sigs.k8s.io/yaml"
)

// maxChartYAML bounds the Chart.yaml read: a chart's metadata is a few hundred
// bytes, and the archive is a build output, but nothing should make this read
// an unbounded entry into memory.
const maxChartYAML = 1 << 20

// Chart is what chartconfig reads out of a packaged chart.
type Chart struct {
	// Name is Chart.yaml's `name:`.
	Name string
	// Version is Chart.yaml's `version:` as packaged.
	Version string
	// Config is Chart.yaml as JSON: the helm OCI config blob.
	Config []byte
}

// Read extracts the top-level `<dir>/Chart.yaml` from a packaged chart (a
// gzipped tar) and converts it. Exactly one such entry must exist: a
// dependency's Chart.yaml lives deeper (`<dir>/charts/<dep>/Chart.yaml`) and is
// never the chart's own.
func Read(r io.Reader) (*Chart, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var raw []byte
	found := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the chart archive: %w", err)
		}
		parts := strings.Split(strings.TrimPrefix(hdr.Name, "./"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] != "Chart.yaml" {
			continue
		}
		found++
		raw, err = io.ReadAll(io.LimitReader(tr, maxChartYAML+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}
		if len(raw) > maxChartYAML {
			return nil, fmt.Errorf("%s is larger than %d bytes", hdr.Name, maxChartYAML)
		}
	}
	switch found {
	case 0:
		return nil, errors.New("no <chart>/Chart.yaml at the top of the archive")
	case 1:
	default:
		return nil, fmt.Errorf("%d top-level Chart.yaml entries; a packaged chart has exactly one", found)
	}
	return Parse(raw)
}

// Parse converts one Chart.yaml document.
func Parse(raw []byte) (*Chart, error) {
	config, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Chart.yaml: %w", err)
	}
	var meta struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(config, &meta); err != nil {
		return nil, fmt.Errorf("the Chart.yaml document is not a mapping: %w", err)
	}
	if meta.Name == "" {
		return nil, errors.New("no name in Chart.yaml")
	}
	if meta.Version == "" {
		return nil, errors.New("no version in Chart.yaml")
	}
	return &Chart{Name: meta.Name, Version: meta.Version, Config: config}, nil
}
