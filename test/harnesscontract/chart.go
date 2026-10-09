package harnesscontract

import (
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Render is one `helm template` of a chart in this repository and the objects
// of that render a harness addresses.
type Render struct {
	ID        string            `json:"id"`
	Chart     string            `json:"chart"`
	Release   string            `json:"release"`
	Namespace string            `json:"namespace"`
	Set       map[string]string `json:"set"`
	Objects   []Object          `json:"objects"`
	CheckedBy string            `json:"checked_by"`
}

// Object is one rendered object. Only what is set is checked.
type Object struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	PodLabels     map[string]string `json:"pod_labels"`
	Containers    []Container       `json:"containers"`
	RollingUpdate *RollingUpdate    `json:"rolling_update"`
}

// Container is a container of the object's pod template.
type Container struct {
	Name string `json:"name"`
	// EnvContains maps an environment variable to substrings its value holds.
	EnvContains map[string][]string `json:"env_contains"`
}

// RollingUpdate is a workload's rolling-update strategy, values as written.
type RollingUpdate struct {
	MaxSurge       string `json:"maxSurge"`
	MaxUnavailable string `json:"maxUnavailable"`
}

// noGeneratedKey is the aether chart value that takes the webhook certificate
// from SPIRE instead of generating a key pair in the render.
const noGeneratedKey = "controller.webhook.spire"

func (r Render) validate() []string {
	var problems []string
	if r.Chart == "" || r.Release == "" || r.Namespace == "" {
		problems = append(problems, fmt.Sprintf("%s needs a chart, a release and a namespace", r.ID))
	}
	if len(r.Objects) == 0 {
		problems = append(problems, fmt.Sprintf("%s lists no object", r.ID))
	}
	// The aether chart generates a webhook private key at render time unless
	// the webhook's certificate comes from SPIRE. No test render needs one.
	if r.Chart == "aether" && r.Set[noGeneratedKey] != "true" {
		problems = append(problems, fmt.Sprintf("%s renders the aether chart without `%s: \"true\"` under set: the render would hold a generated private key", r.ID, noGeneratedKey))
	}
	for _, o := range r.Objects {
		if o.ID == "" || o.Kind == "" || o.Name == "" {
			problems = append(problems, fmt.Sprintf("%s: an object needs an id, a kind and a name (got id=%q kind=%q name=%q)", r.ID, o.ID, o.Kind, o.Name))
		}
	}
	return problems
}

// HelmArgs is the `helm` command line that produces the render from the
// packaged chart at chartPath. The --set pairs are sorted, so it is the same
// command every time.
func (r Render) HelmArgs(chartPath string) []string {
	args := []string{"template", r.Release, chartPath, "--namespace", r.Namespace}
	for _, k := range sortedKeys(r.Set) {
		args = append(args, "--set", k+"="+r.Set[k])
	}
	return args
}

// RendersOf returns the contract's renders of the chart called name.
func (c *Contract) RendersOf(name string) []Render {
	var out []Render
	for _, r := range c.Charts {
		if r.Chart == name {
			out = append(out, r)
		}
	}
	return out
}

// manifest is the part of a rendered object the contract can speak about.
type manifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		// A DaemonSet's is updateStrategy, a Deployment's is strategy.
		UpdateStrategy *strategy `json:"updateStrategy"`
		Strategy       *strategy `json:"strategy"`
		Template       struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				InitContainers []container `json:"initContainers"`
				Containers     []container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type strategy struct {
	RollingUpdate *struct {
		MaxSurge       any `json:"maxSurge"`
		MaxUnavailable any `json:"maxUnavailable"`
	} `json:"rollingUpdate"`
}

type container struct {
	Name string `json:"name"`
	Env  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

// documents splits a `helm template` output at its `---` lines.
func documents(render []byte) []string {
	var out []string
	var doc []string
	for line := range strings.SplitSeq(string(render), "\n") {
		if line == "---" || strings.HasPrefix(line, "--- ") {
			out = append(out, strings.Join(doc, "\n"))
			doc = doc[:0]
			continue
		}
		doc = append(doc, line)
	}
	return append(out, strings.Join(doc, "\n"))
}

// parseRender reads the objects of a `helm template` output. A document that
// is not a mapping with a kind (a comment-only document, NOTES) is left out.
func parseRender(render []byte) ([]manifest, error) {
	var out []manifest
	for _, text := range documents(render) {
		if strings.TrimSpace(text) == "" {
			continue
		}
		var m manifest
		if err := yaml.Unmarshal([]byte(text), &m); err != nil {
			// Never the document itself: a render can hold generated key material.
			return nil, fmt.Errorf("a document of the render is not YAML this check can read (%d lines, not shown)", strings.Count(text, "\n")+1)
		}
		if m.Kind != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// Check compares the render (the output of `helm` run with r.HelmArgs) with
// the objects the contract lists, and returns what differs. It never returns
// any part of the render other than the names of objects and containers, pod
// labels, and the names of environment variables: a chart can generate key
// material at render time.
func (r Render) Check(render []byte) []string {
	docs, err := parseRender(render)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", r.ID, err)}
	}
	var problems []string
	for _, o := range r.Objects {
		problems = append(problems, o.check(docs)...)
	}
	return problems
}

// find returns the one rendered object of o's kind and name, or why there is
// not exactly one.
func (o Object) find(docs []manifest) (manifest, string) {
	var found []manifest
	var sameKind []string
	for _, d := range docs {
		if d.Kind != o.Kind {
			continue
		}
		sameKind = append(sameKind, d.Metadata.Name)
		if d.Metadata.Name == o.Name {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 1:
		return found[0], ""
	case 0:
		slices.Sort(sameKind)
		return manifest{}, fmt.Sprintf("is not rendered (the render's %s objects: %s)", o.Kind, orNone(sameKind))
	}
	return manifest{}, fmt.Sprintf("is rendered %d times", len(found))
}

func (o Object) check(docs []manifest) []string {
	what := fmt.Sprintf("%s: %s/%s", o.ID, o.Kind, o.Name)
	d, problem := o.find(docs)
	if problem != "" {
		return []string{what + " " + problem}
	}
	var problems []string
	if o.Namespace != "" && d.Metadata.Namespace != o.Namespace {
		problems = append(problems, fmt.Sprintf("%s is in namespace %q, the contract says %q", what, d.Metadata.Namespace, o.Namespace))
	}
	labels := d.Spec.Template.Metadata.Labels
	for _, k := range sortedKeys(o.PodLabels) {
		if got, ok := labels[k]; !ok || got != o.PodLabels[k] {
			problems = append(problems, fmt.Sprintf("%s has no pod label %s=%s (its pod labels: %s)", what, k, o.PodLabels[k], orNone(labelPairs(labels))))
		}
	}
	for _, c := range o.Containers {
		problems = append(problems, c.check(what, d.Spec.Template.Spec.Containers)...)
	}
	if o.RollingUpdate != nil {
		if got := rollingUpdateOf(d); got != *o.RollingUpdate {
			problems = append(problems, fmt.Sprintf("%s rolls with maxSurge=%s maxUnavailable=%s, the contract says maxSurge=%s maxUnavailable=%s",
				what, orUnset(got.MaxSurge), orUnset(got.MaxUnavailable), o.RollingUpdate.MaxSurge, o.RollingUpdate.MaxUnavailable))
		}
	}
	return problems
}

func (c Container) check(what string, containers []container) []string {
	var names []string
	for _, got := range containers {
		names = append(names, got.Name)
		if got.Name == c.Name {
			return c.checkEnv(what, got)
		}
	}
	return []string{fmt.Sprintf("%s has no container %q (its containers: %s)", what, c.Name, orNone(names))}
}

func (c Container) checkEnv(what string, got container) []string {
	var problems []string
	for _, env := range sortedKeys(c.EnvContains) {
		value := ""
		for _, e := range got.Env {
			if e.Name == env {
				value = e.Value
			}
		}
		for _, want := range c.EnvContains[env] {
			if !strings.Contains(value, want) {
				// Not the value: an environment variable can hold a secret.
				problems = append(problems, fmt.Sprintf("%s container %q: %s does not contain %q", what, c.Name, env, want))
			}
		}
	}
	return problems
}

func rollingUpdateOf(d manifest) RollingUpdate {
	s := d.Spec.UpdateStrategy
	if s == nil {
		s = d.Spec.Strategy
	}
	if s == nil || s.RollingUpdate == nil {
		return RollingUpdate{}
	}
	text := func(v any) string {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	return RollingUpdate{MaxSurge: text(s.RollingUpdate.MaxSurge), MaxUnavailable: text(s.RollingUpdate.MaxUnavailable)}
}

func labelPairs(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		out = append(out, k+"="+labels[k])
	}
	return out
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
