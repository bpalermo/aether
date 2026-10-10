// Package render runs `helm template` over a packaged chart and returns the
// objects of the render, for a chart check written in Go: one that compares
// what a chart renders with a Go constant, which a pattern in a BUILD file
// cannot do (//bazel/helm/meshnames).
//
// Nothing here prints a render: with default values the aether chart
// generates the webhook's private key at render time (#1382). When helm fails,
// only its exit status is returned: nothing it wrote, on either stream. Helm
// can put manifest text on stderr (a YAML parse error quotes the document), and
// the pattern rules pass it through a fail-closed mask for that reason
// (render_lib.sh, show_helm_failure). This package has no mask, so it shows
// none of it. A document that does not parse is reported without its text.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Meta is the part of an object's (or a pod template's) metadata a chart check
// compares.
type Meta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// Volume is a pod volume, with the one source a chart check looks at.
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
		// Containers is what makes a `template` a pod template: see
		// podTemplate.
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
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

// Helm is the toolchain's helm: the binary and its plugins directory.
type Helm struct {
	Binary  string
	Plugins string
}

// Template runs `helm template release-name <chart> <opts>` and returns the
// objects of the render, in the order helm printed them. scratch is a
// directory helm may write to: it keeps its caches, repository list and
// registry credentials under $HOME, and a sandbox has no writable home.
func (h Helm) Template(chart, scratch string, opts ...string) ([]Object, error) {
	cmd := exec.Command(h.Binary, append([]string{"template", "release-name", chart}, opts...)...)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(scratch, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(scratch, "config"),
		"HELM_DATA_HOME="+filepath.Join(scratch, "data"),
		"HELM_REPOSITORY_CACHE="+filepath.Join(scratch, "repository_cache"),
		"HELM_REPOSITORY_CONFIG="+filepath.Join(scratch, "repositories.yaml"),
		"HELM_REGISTRY_CONFIG="+filepath.Join(scratch, "registry.json"),
		"HELM_PLUGINS="+h.Plugins,
	)
	// Stderr is not captured at all (a nil Stderr is the null device), so it
	// cannot be printed by a later change to the message either.
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// %v of the exit error is "exit status N" (or why helm could not be
		// started): it holds nothing helm wrote.
		return nil, fmt.Errorf("helm template %s failed: %v. What helm wrote is not shown: a failed render can hold key material. "+
			"To read it, run it yourself: `bazel build` the chart and `helm template release-name <the .tgz> %s`", strings.Join(opts, " "), err, strings.Join(opts, " "))
	}
	objects, err := Parse(stdout.String())
	if err != nil {
		return nil, fmt.Errorf("helm template %s: %w", strings.Join(opts, " "), err)
	}
	return objects, nil
}

// Parse splits a multi-document render into its objects. A document with no
// `kind` (an empty one, or only comments) is skipped.
func Parse(render string) ([]Object, error) {
	var objects []Object
	for i, doc := range strings.Split("\n"+render, "\n---") {
		var d document
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			// Neither the document nor the decoder's message, which can
			// quote the value it failed on, is shown: it may be the Secret.
			return nil, fmt.Errorf("document %d of the render is not YAML this package can read (%T; text withheld: a render can hold key material)", i, err)
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
		return nil, errors.New("the render holds no object")
	}
	return objects, nil
}

// podTemplate decodes spec.template when it is a pod template, and returns nil
// when it is something else: another kind's field of the same name, which may
// be a scalar or a mapping of its own (a custom resource's `template: {}`).
// Any mapping decodes into PodTemplate, so the shape is checked: a pod
// template has at least one container.
func podTemplate(template any) *PodTemplate {
	raw, err := yaml.Marshal(template)
	if err != nil {
		return nil
	}
	var pt PodTemplate
	if err := yaml.Unmarshal(raw, &pt); err != nil {
		return nil
	}
	if len(pt.Spec.Containers) == 0 {
		return nil
	}
	return &pt
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
