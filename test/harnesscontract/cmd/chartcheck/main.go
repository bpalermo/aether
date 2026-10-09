// Command chartcheck renders one chart the way the external-harness contract
// says (test/harnesscontract/external-harness.yaml, `charts`) and compares
// the render with the objects the contract lists. The helm_contract_test rule
// (test/harnesscontract/defs.bzl) runs it with the toolchain's helm and the
// packaged chart.
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
	"strings"

	"aethermesh.dev/test/harnesscontract"
)

func main() {
	helm := flag.String("helm", "", "path to the helm binary")
	chart := flag.String("chart", "", "path to the packaged chart")
	name := flag.String("name", "", "the chart's name in the contract")
	target := flag.String("target", "", "this test's Bazel label, as a checked_by in the contract spells it")
	ids := flag.String("ids", "", "the ids of the renders and objects this test holds, comma-separated")
	flag.Parse()
	os.Exit(run(os.Stdout, os.Stderr, *helm, *chart, *name, *target, strings.Split(*ids, ","), helmTemplate))
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

func run(stdout, stderr io.Writer, helm, chart, name, target string, ids []string, render renderFunc) int {
	if helm == "" || chart == "" || name == "" || target == "" {
		fmt.Fprintln(stderr, "usage: chartcheck --helm HELM --chart CHART.tgz --name NAME --target //PACKAGE:TEST --ids ID,ID,...")
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
	for _, r := range renders {
		args := r.HelmArgs(chart)
		out, errOut, err := render(helm, args)
		if err != nil {
			fmt.Fprintf(stderr, "FAIL: %s: `helm %s` did not render: %v\n----- what helm said (its stderr) -----\n%s\n",
				r.ID, strings.Join(args, " "), err, errOut)
			failed = true
			continue
		}
		problems := r.Check(out)
		for _, p := range problems {
			fmt.Fprintf(stderr, "FAIL: %s (render %s: helm %s)\n", p, r.ID, strings.Join(args, " "))
		}
		if len(problems) > 0 {
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "PASS: %s: the %d objects the contract lists are rendered as it says.\n", r.ID, len(r.Objects))
	}
	if failed {
		fmt.Fprintf(stderr, "\nThe chart %q no longer renders what %s says a harness outside this repository may address.\n%s\n",
			name, harnesscontract.File, harnesscontract.Rule)
		return 1
	}
	return 0
}
