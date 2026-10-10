// Command chartcheck renders one chart the way the external-harness contract
// says (test/harnesscontract/external-harness.yaml, `charts`) and compares
// the render with the objects the contract lists. The helm_contract_test rule
// (test/harnesscontract/defs.bzl) runs it with the toolchain's helm and the
// packaged chart, or with every packaging of it when the chart is published
// under more than one (the aether chart's `X.Y.Z` and `X.Y.Z-<sha>`): each
// package is rendered and compared on its own.
//
// It never prints the render: a chart can generate key material at render
// time. On a helm failure it prints helm's error output only.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"aethermesh.dev/test/harnesscontract"
)

func main() {
	helm := flag.String("helm", "", "path to the helm binary")
	chart := flag.String("chart", "", "path to the packaged chart; comma-separated when the chart has several packagings")
	name := flag.String("name", "", "the chart's name in the contract")
	target := flag.String("target", "", "this test's Bazel label, as a checked_by in the contract spells it")
	ids := flag.String("ids", "", "the ids of the renders and objects this test holds, comma-separated")
	flag.Parse()
	os.Exit(run(os.Stdout, os.Stderr, *helm, strings.Split(*chart, ","), *name, *target, strings.Split(*ids, ","), helmTemplate))
}

// renderFunc runs helm with args and returns its standard output and its
// standard error.
type renderFunc func(helm string, args []string) (stdout, stderr []byte, err error)

func helmTemplate(helm string, args []string) ([]byte, []byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command(helm, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// check renders one packaging of the chart as the contract's render r says and
// compares. It reports whether the render is what the contract lists.
func check(stdout, stderr io.Writer, helm, chart string, r harnesscontract.Render, render renderFunc) bool {
	args := r.HelmArgs(chart)
	out, errOut, err := render(helm, args)
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: %s: `helm %s` did not render: %v\n----- what helm said (its stderr) -----\n%s\n",
			r.ID, strings.Join(args, " "), err, errOut)
		return false
	}
	problems := r.Check(out)
	for _, p := range problems {
		fmt.Fprintf(stderr, "FAIL: %s (render %s: helm %s)\n", p, r.ID, strings.Join(args, " "))
	}
	if len(problems) > 0 {
		return false
	}
	fmt.Fprintf(stdout, "PASS: %s (%s): the %d objects the contract lists are rendered as it says.\n", r.ID, chart, len(r.Objects))
	return true
}

func run(stdout, stderr io.Writer, helm string, charts []string, name, target string, ids []string, render renderFunc) int {
	if helm == "" || len(charts) == 0 || slices.Contains(charts, "") || name == "" || target == "" {
		fmt.Fprintln(stderr, "usage: chartcheck --helm HELM --chart CHART.tgz[,CHART.tgz...] --name NAME --target //PACKAGE:TEST --ids ID,ID,...")
		return 2
	}
	contract, err := harnesscontract.Load()
	if err != nil {
		fmt.Fprintln(stderr, "FAIL:", err)
		return 1
	}
	renders := contract.RendersOf(name)
	if len(renders) == 0 {
		fmt.Fprintf(stderr, "FAIL: %s has no entry under `charts` for the chart %q, and this test exists to check one.\n%s\n",
			harnesscontract.File, name, harnesscontract.Rule)
		return 1
	}
	failed := false
	// The test's own list of what it holds, against the contract's: a render or
	// an object removed from the contract while the chart still renders it must
	// not pass for want of anything to compare.
	for _, p := range contract.HeldByChartTest(target, name, ids) {
		fmt.Fprintln(stderr, "FAIL:", p)
		failed = true
	}
	// Every packaging is held to every render: what is deployed is one of the
	// packages, and which one is the installer's choice.
	for _, chart := range charts {
		for _, r := range renders {
			if !check(stdout, stderr, helm, chart, r, render) {
				failed = true
			}
		}
	}
	if failed {
		fmt.Fprintf(stderr, "\nThe chart %q no longer renders what %s says a harness outside this repository may address.\n%s\n",
			name, harnesscontract.File, harnesscontract.Rule)
		return 1
	}
	return 0
}
