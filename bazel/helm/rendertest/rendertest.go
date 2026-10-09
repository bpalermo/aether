// Package rendertest renders a packaged Helm chart inside a Bazel Go test and
// hands the test the objects, so a chart test can compare what a chart renders
// with a Go constant or with a file instead of with a literal in a BUILD file.
//
// A go_test that calls Render declares the chart and the Helm toolchain:
//
//	go_test(
//	    name = "prober_test",
//	    srcs = ["mesh_names_test.go"],
//	    data = [":prober", "@rules_helm//helm:current_toolchain"],
//	    env = {
//	        "RENDERTEST_CHART": "$(rootpath :prober)",
//	        "RENDERTEST_HELM": "$(rootpaths @rules_helm//helm:current_toolchain)",
//	    },
//	    deps = ["//bazel/helm/rendertest"],
//	)
//
// Render never prints a render: with default values the aether chart generates
// the webhook's private key at render time (#1382). When helm fails, only what
// it wrote to stderr is shown.
package rendertest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	// chartEnv holds the packaged chart, as a path relative to the workspace
	// root ($(rootpath <the helm_chart target>)).
	chartEnv = "RENDERTEST_CHART"
	// helmEnv holds the files of @rules_helm//helm:current_toolchain
	// ($(rootpaths ...)): the helm binary and its plugins directory.
	helmEnv = "RENDERTEST_HELM"
)

// Meta is the part of an object's (or a pod template's) metadata a chart test
// compares.
type Meta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// Volume is a pod volume, with the one source a chart test looks at.
type Volume struct {
	Name string `json:"name"`
	CSI  *struct {
		Driver string `json:"driver"`
	} `json:"csi"`
}

// PodTemplate is spec.template of a workload.
type PodTemplate struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Volumes []Volume `json:"volumes"`
	} `json:"spec"`
}

// Object is one document of a render. Template is nil for an object that has
// no pod template.
type Object struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Meta         `json:"metadata"`
	Template   *PodTemplate `json:"-"`
}

// ID is the object's "<Kind>/<name>".
func (o Object) ID() string { return o.Kind + "/" + o.Metadata.Name }

// document is what an Object is decoded from: spec is kept loose, because only
// a workload's spec has a pod template and another kind's `template` may be
// anything.
type document struct {
	Object
	Spec map[string]any `json:"spec"`
}

// Render runs `helm template release-name <chart> <opts>` with the toolchain's
// helm and returns the objects of the render, in the order helm printed them.
func Render(t testing.TB, opts ...string) []Object {
	t.Helper()
	helm, plugins := toolchain(t)
	chart := workspacePath(t, mustEnv(t, chartEnv))

	// Helm keeps its caches, repository list and registry credentials under
	// $HOME; a sandbox has no writable home.
	scratch := t.TempDir()
	cmd := exec.Command(helm, append([]string{"template", "release-name", chart}, opts...)...)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(scratch, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(scratch, "config"),
		"HELM_DATA_HOME="+filepath.Join(scratch, "data"),
		"HELM_REPOSITORY_CACHE="+filepath.Join(scratch, "repository_cache"),
		"HELM_REPOSITORY_CONFIG="+filepath.Join(scratch, "repositories.yaml"),
		"HELM_REGISTRY_CONFIG="+filepath.Join(scratch, "registry.json"),
		"HELM_PLUGINS="+plugins,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %s failed: %v\n----- what helm said (its stderr) -----\n%s", strings.Join(opts, " "), err, stderr.String())
	}
	objects, err := Parse(stdout.String())
	if err != nil {
		t.Fatalf("helm template %s: %v", strings.Join(opts, " "), err)
	}
	return objects
}

// Parse splits a multi-document render into its objects. A document with no
// `kind` (an empty one, or only comments) is skipped.
func Parse(render string) ([]Object, error) {
	var objects []Object
	for i, doc := range strings.Split("\n"+render, "\n---") {
		var d document
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			// The document is not shown: it may be the Secret.
			return nil, fmt.Errorf("document %d of the render is not YAML this package can read: %w", i, errWithoutContent(err))
		}
		if d.Kind == "" {
			continue
		}
		object := d.Object
		if template, ok := d.Spec["template"]; ok {
			object.Template = podTemplate(template)
		}
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("the render holds no object")
	}
	return objects, nil
}

// errWithoutContent keeps an unmarshal error's kind and drops its text, which
// can quote the value that failed to decode.
func errWithoutContent(err error) error {
	return fmt.Errorf("%T (text withheld: a render can hold key material)", err)
}

// podTemplate decodes spec.template when it is a pod template, and returns nil
// when it is something else (another kind's field of the same name).
func podTemplate(template any) *PodTemplate {
	raw, err := yaml.Marshal(template)
	if err != nil {
		return nil
	}
	var pt PodTemplate
	if err := yaml.Unmarshal(raw, &pt); err != nil {
		return nil
	}
	return &pt
}

// Find returns the object with that "<Kind>/<name>", and fails the test with
// the list of what the render holds when there is none or more than one.
func Find(t testing.TB, objects []Object, id string) Object {
	t.Helper()
	var found []Object
	var ids []string
	for _, o := range objects {
		ids = append(ids, o.ID())
		if o.ID() == id {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the render holds %d objects named %s, want 1; it holds: %s", len(found), id, strings.Join(ids, ", "))
	}
	return found[0]
}

// Workloads returns the objects that have a pod template.
func Workloads(objects []Object) []Object {
	var out []Object
	for _, o := range objects {
		if o.Template != nil {
			out = append(out, o)
		}
	}
	return out
}

// toolchain returns the helm binary and its plugins directory.
func toolchain(t testing.TB) (helm, plugins string) {
	t.Helper()
	for _, f := range strings.Fields(mustEnv(t, helmEnv)) {
		path := workspacePath(t, f)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s names %s, which is not in the test's runfiles: %v", helmEnv, f, err)
		}
		if info.IsDir() {
			plugins = path
		} else {
			helm = path
		}
	}
	if helm == "" || plugins == "" {
		t.Fatalf("%s=%q does not hold a helm binary and a plugins directory: set it to $(rootpaths @rules_helm//helm:current_toolchain)", helmEnv, os.Getenv(helmEnv))
	}
	return helm, plugins
}

func mustEnv(t testing.TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: see the package comment of //bazel/helm/rendertest for the go_test attributes", name)
	}
	return v
}

// workspacePath turns a $(rootpath) (relative to the workspace root; a file of
// another repository starts with "../") into an absolute path in the runfiles.
func workspacePath(t testing.TB, rootpath string) string {
	t.Helper()
	srcdir, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE")
	if srcdir == "" || workspace == "" {
		t.Fatalf("TEST_SRCDIR and TEST_WORKSPACE are not set: this test runs under `bazel test`")
	}
	return filepath.Join(srcdir, workspace, rootpath)
}
