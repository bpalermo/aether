// Command generate-envoy-bootstrap emits representative Envoy bootstrap JSON
// files into --out for offline inspection or CI artifact storage.
//
// The bootstrap configs are the same as those validated by
// //test/envoy_validate:envoy_validate_test.  Running the generator standalone
// is useful for inspecting the produced JSON before changing the proxy builders.
//
// Usage:
//
//	bazel run //test/envoy_validate/generate -- --out /tmp/envoy-validate
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	envoy_validate "github.com/bpalermo/aether/test/envoy_validate"
)

func main() {
	outDir := flag.String("out", ".", "directory to write bootstrap JSON files into")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	tasks := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"node_bootstrap.json", envoy_validate.NodeBootstrapJSON},
		{"capture_bootstrap.json", envoy_validate.CaptureBootstrapJSON},
		{"capture_route_target_bootstrap.json", envoy_validate.CaptureRouteTargetBootstrapJSON},
		{"edge_bootstrap.json", envoy_validate.EdgeBootstrapJSON},
	}

	for _, t := range tasks {
		data, err := t.fn()
		if err != nil {
			log.Fatalf("build %s: %v", t.name, err)
		}
		out := filepath.Join(*outDir, t.name)
		if err := os.WriteFile(out, data, 0o644); err != nil {
			log.Fatalf("write %s: %v", t.name, err)
		}
		log.Printf("wrote %s", out)
	}
}
