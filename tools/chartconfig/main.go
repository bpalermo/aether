package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	in := flag.String("chart", "", "the packaged chart (.tgz) to read")
	configOut := flag.String("config_out", "", "where to write the helm OCI config (Chart.yaml as JSON)")
	metaOut := flag.String("meta_out", "", "where to write `<name> <version>` (one line)")
	flag.Parse()
	if *in == "" || *configOut == "" || *metaOut == "" {
		fmt.Fprintln(os.Stderr, "usage: chartconfig -chart <pkg.tgz> -config_out <file> -meta_out <file>")
		os.Exit(2)
	}
	if err := run(*in, *configOut, *metaOut); err != nil {
		fmt.Fprintf(os.Stderr, "chartconfig: %s: %v\n", *in, err)
		os.Exit(1)
	}
}

func run(in, configOut, metaOut string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	chart, err := Read(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configOut, chart.Config, 0o644); err != nil {
		return err
	}
	return os.WriteFile(metaOut, fmt.Appendf(nil, "%s %s\n", chart.Name, chart.Version), 0o644)
}
