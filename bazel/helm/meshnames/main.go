// meshnames holds the mesh names a chart writes on its OWN pods to the code
// that reads them (#1589). It is the checker behind helm_mesh_names_test
// (//bazel/helm:defs.bzl).
//
// charts/prober and charts/udsecho spell the mesh-managed label, the mesh
// annotations and the CSI driver of the socket volume as literals in their
// templates. A pattern test compares a literal in a BUILD file with a literal
// in a template; both keep agreeing while the constant the CNI plugin, the
// agent and the node plugin read has been renamed, and the chart's pods are
// then unmeshed, or without their upstreams or their socket volume, with every
// test passing. So the chart's BUILD file names what each workload's pod must
// carry by SYMBOL ("upstreams", "endpoint_port=8080", "csi") and this program
// resolves the symbols to the Go constants.
//
// It is a program a test rule runs, not a go_test, on purpose: a go_test with
// a chart in its `data` cannot be analysed under --config=race (the chart's
// images are linked with cgo off), and the race job runs every go_test.
//
// It never prints a render.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"aethermesh.dev/bazel/helm/render"
	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/udspath"
	"aethermesh.dev/test/harnesscontract"
)

// csiSymbol asks for exactly one volume of the mesh's CSI driver
// (udspath.CSIDriver, the name the agent resolves a socket request against and
// the node plugin registers). A workload without it must have no CSI volume.
const csiSymbol = "csi"

// upstreamsID is the external-harness contract entry for
// config.aether.io/upstreams. The Go constant is internal to the agent
// (agent/internal/xds/xdsconst), which nothing outside agent/ may import;
// //agent/internal/xds/xdsconst:xdsconst_test holds that constant to this
// entry, and this program holds the charts to the same entry.
const upstreamsID = "pod.annotation.upstreams"

// annotationSymbols maps the symbols a BUILD file may use to the annotation
// key the code reads.
func annotationSymbols() (map[string]string, error) {
	contract, err := harnesscontract.Load()
	if err != nil {
		return nil, err
	}
	for _, n := range contract.Names {
		if n.ID == upstreamsID {
			return map[string]string{
				"upstreams":     n.Value,
				"endpoint_port": annotations.AnnotationEndpointPort,
				"uds_socket":    annotations.AnnotationEndpointUDSSocket,
			}, nil
		}
	}
	return nil, fmt.Errorf("%s has no entry %s", harnesscontract.File, upstreamsID)
}

// want is what one workload's pod template must carry besides the mesh label.
type want struct {
	// annotations: key -> value; "" when only the key's presence is checked.
	annotations map[string]string
	csi         bool
}

// parseWorkloads turns the listed workloads ("<Kind>/<name>") and what each
// carries ("<Kind>/<name>=<symbol>[=<value>]", one item per argument, so a
// value may hold a comma) into expectations. A workload with no item carries
// the mesh label only.
func parseWorkloads(workloads, carries []string, symbols map[string]string) (map[string]want, error) {
	out := map[string]want{}
	for _, id := range workloads {
		if id == "" || strings.Contains(id, "=") {
			return nil, fmt.Errorf("--workload %q: want <Kind>/<name>", id)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("--workload %s is given twice", id)
		}
		out[id] = want{annotations: map[string]string{}}
	}
	for _, arg := range carries {
		id, item, ok := strings.Cut(arg, "=")
		w, listed := out[id]
		if !ok || item == "" || !listed {
			return nil, fmt.Errorf("--carries %q: want <Kind>/<name>=<symbol>[=<value>] for a workload given with --workload", arg)
		}
		symbol, value, _ := strings.Cut(item, "=")
		if symbol == csiSymbol {
			w.csi = true
			out[id] = w
			continue
		}
		key, known := symbols[symbol]
		if !known {
			return nil, fmt.Errorf("--carries %s: unknown symbol %q (known: %s, %s)", id, symbol, strings.Join(sortedKeys(symbols), ", "), csiSymbol)
		}
		w.annotations[key] = value
	}
	return out, nil
}

// check compares every workload of the render with what is wanted of it. The
// comparison is closed in both directions: a workload nobody listed, a listed
// workload that is not rendered, and a label or annotation in the mesh's
// namespaces that no symbol stands for are all problems.
func check(objects []render.Object, wanted map[string]want) []string {
	var problems []string
	seen := map[string]bool{}
	for _, w := range render.Workloads(objects) {
		expect, ok := wanted[w.ID()]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is a workload the test does not list: add it, with the mesh names its pods carry", w.ID()))
			continue
		}
		seen[w.ID()] = true
		problems = append(problems, checkWorkload(w, expect)...)
	}
	for _, id := range sortedKeys(wanted) {
		if !seen[id] {
			problems = append(problems, fmt.Sprintf("%s is listed and the chart does not render it as a workload", id))
		}
	}
	return problems
}

func checkWorkload(w render.Object, expect want) []string {
	var problems []string
	meta := w.Template.Metadata
	if got := meta.Labels[labels.LabelAetherManaged]; got != "true" {
		problems = append(problems, fmt.Sprintf("%s: pod label %s=%q, want \"true\" (labels.LabelAetherManaged): the pod would not be in the mesh. Its mesh labels: %v",
			w.ID(), labels.LabelAetherManaged, got, meshKeys(meta.Labels)))
	}
	for _, key := range sortedKeys(expect.annotations) {
		value := expect.annotations[key]
		got, ok := meta.Annotations[key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: no pod annotation %s. Its mesh annotations: %v", w.ID(), key, meshKeys(meta.Annotations)))
		case value != "" && got != value:
			problems = append(problems, fmt.Sprintf("%s: pod annotation %s=%q, want %q", w.ID(), key, got, value))
		}
	}
	// Closed: a mesh name on the pod that no constant stands for is a literal
	// that drifted, or a new one the BUILD file has to list.
	for _, key := range meshKeys(meta.Labels) {
		if key != labels.LabelAetherManaged {
			problems = append(problems, fmt.Sprintf("%s: pod label %s is not a mesh label the code knows", w.ID(), key))
		}
	}
	for _, key := range meshKeys(meta.Annotations) {
		if _, ok := expect.annotations[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s: pod annotation %s is not one the test lists for this workload, or not one the code knows", w.ID(), key))
		}
	}
	return append(problems, checkCSI(w, expect.csi)...)
}

func checkCSI(w render.Object, wantCSI bool) []string {
	var drivers []string
	for _, v := range w.Template.Spec.Volumes {
		if v.CSI != nil {
			drivers = append(drivers, v.CSI.Driver)
		}
	}
	switch {
	case !wantCSI && len(drivers) != 0:
		return []string{fmt.Sprintf("%s: CSI volumes of %v, and the test lists none (%q)", w.ID(), drivers, csiSymbol)}
	case wantCSI && (len(drivers) != 1 || drivers[0] != udspath.CSIDriver):
		return []string{fmt.Sprintf("%s: CSI volume drivers %v, want exactly one volume of %s (udspath.CSIDriver)", w.ID(), drivers, udspath.CSIDriver)}
	}
	return nil
}

// meshKeys returns the keys in the mesh's namespaces (aether.io and its
// subdomains), sorted.
func meshKeys(m map[string]string) []string {
	var keys []string
	for k := range m {
		prefix, _, _ := strings.Cut(k, "/")
		if prefix == "aether.io" || strings.HasSuffix(prefix, ".aether.io") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// multi is a flag that may be given more than once.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, " ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(args []string) error {
	fs := flag.NewFlagSet("meshnames", flag.ContinueOnError)
	helm := fs.String("helm", "", "the helm binary")
	plugins := fs.String("helm-plugins", "", "the helm plugins directory")
	chart := fs.String("chart", "", "the packaged chart")
	scratch := fs.String("scratch", "", "a directory helm may write to")
	var workloads, carries, opts multi
	fs.Var(&workloads, "workload", "<Kind>/<name> of a workload the render holds (repeatable)")
	fs.Var(&carries, "carries", "<Kind>/<name>=<symbol>[=<value>]: one mesh name that workload's pod carries (repeatable)")
	fs.Var(&opts, "opt", "one argument for `helm template` (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *helm == "" || *chart == "" || *scratch == "" || len(workloads) == 0 {
		return fmt.Errorf("--helm, --chart, --scratch and at least one --workload are required")
	}

	symbols, err := annotationSymbols()
	if err != nil {
		return err
	}
	wanted, err := parseWorkloads(workloads, carries, symbols)
	if err != nil {
		return err
	}
	objects, err := render.Helm{Binary: *helm, Plugins: *plugins}.Template(*chart, *scratch, opts...)
	if err != nil {
		return err
	}
	if problems := check(objects, wanted); len(problems) != 0 {
		return fmt.Errorf("the chart's pods and the mesh names the code reads disagree:\n  %s", strings.Join(problems, "\n  "))
	}
	fmt.Printf("PASS: %d workloads carry the mesh names the code reads.\n", len(wanted))
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}
