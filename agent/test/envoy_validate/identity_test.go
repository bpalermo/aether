package envoy_validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvoyVersionRejectsNonEnvoy is the red half of the override's proof: the
// gate can be pointed at any binary (validate-built-proxy.sh), so it must fail
// on one that is not an Envoy — including a stand-in that exits 0 and would
// otherwise pass every positive `--mode validate` case.
func TestEnvoyVersionRejectsNonEnvoy(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"exits-nonzero":    "#!/bin/sh\nexit 1\n",
		"exits-zero-quiet": "#!/bin/sh\nexit 0\n",
		"wrong-output":     "#!/bin/sh\necho 'GNU coreutils 9.4'\n",
	} {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(dir, name)
			if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			if v, err := envoyVersion(bin); err == nil {
				t.Fatalf("envoyVersion(%s) = %q, want an error", name, v)
			}
		})
	}
}

// TestEnvoyVersionAcceptsEnvoyLine pins the line format the gate logs.
func TestEnvoyVersionAcceptsEnvoyLine(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "envoy")
	line := "\n/usr/local/bin/envoy  version: 064a666137f12f56cf6d849fc5dc696ee32398a0/1.40.0-dev/Clean/RELEASE/BoringSSL\n\n"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := envoyVersion(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "version: 064a666137f12f56cf6d849fc5dc696ee32398a0/1.40.0-dev") {
		t.Fatalf("envoyVersion = %q", v)
	}
}
