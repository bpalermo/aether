package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHelm writes a stand-in for the helm binary: a shell script.
func fakeHelm(t *testing.T, script string) Helm {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return Helm{Binary: path, Plugins: t.TempDir()}
}

// keyMaterial is what a failed `helm template` of the aether chart can hold:
// the Secret of the webhook's generated certificate. Helm prints a render to
// stdout, and can put manifest text on stderr too (a YAML parse error quotes
// the document; --debug prints the manifest). The words are split so this
// file holds nothing a secret scanner matches.
const keyMaterial = "kind: Secret\n" +
	"data:\n" +
	"  tls.key: U0VOVElORUwtS0VZ\n" +
	"-----BEGIN RSA " + "PRIVATE KEY-----\n" +
	"SENTINEL-KEY-LINE\n"

// A failed render is reported by its exit status alone. Nothing helm wrote, on
// either stream, reaches the error: the pattern rules pass helm's stderr
// through a fail-closed mask (render_lib.sh, show_helm_failure), and this
// package has no mask, so it shows none of it (#1382).
func TestTemplateWithholdsWhatHelmWroteWhenItFails(t *testing.T) {
	helm := fakeHelm(t, "cat <<'EOF'\n"+keyMaterial+"EOF\ncat >&2 <<'EOF'\nError: YAML parse error on aether/templates/controller-webhook.yaml\n"+keyMaterial+"EOF\nexit 3\n")
	_, err := helm.Template("chart.tgz", t.TempDir(), "--set", "a=b")
	if err == nil {
		t.Fatal("a helm that exits 3 was reported as a successful render")
	}
	for _, leaked := range []string{"SENTINEL", "PRIVATE KEY", "tls.key", "U0VOVElORUw", "kind: Secret", "YAML parse error"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the error holds %q, which only helm's output had", leaked)
		}
	}
	for _, said := range []string{"exit status 3", "--set a=b", "run it yourself"} {
		if !strings.Contains(err.Error(), said) {
			t.Errorf("the error does not say %q: %v", said, err)
		}
	}
}

// A render that succeeds and does not parse is withheld as well.
func TestTemplateWithholdsARenderItCannotParse(t *testing.T) {
	helm := fakeHelm(t, "cat <<'EOF'\nkind: Secret\ndata:\n  tls.key: [SENTINEL-KEY-LINE\nEOF\n")
	_, err := helm.Template("chart.tgz", t.TempDir())
	if err == nil {
		t.Fatal("malformed YAML was reported as a successful render")
	}
	for _, leaked := range []string{"SENTINEL", "tls.key"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the error holds %q, which only the render had", leaked)
		}
	}
}

func TestTemplateReturnsTheObjects(t *testing.T) {
	helm := fakeHelm(t, "cat <<'EOF'\n"+rendered+"EOF\n")
	objects, err := helm.Template("chart.tgz", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(Workloads(objects)) != 1 {
		t.Fatalf("workloads = %v, want one", Workloads(objects))
	}
}
