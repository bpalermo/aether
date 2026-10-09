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
	// ResourceAttributes are ids of `resource_attributes` entries the container
	// is given in OTEL_RESOURCE_ATTRIBUTES. The key (and the value, when the
	// entry has one) is read from that entry, so the two cannot disagree.
	ResourceAttributes []string `json:"resource_attributes"`
	// Args maps a command-line flag to the id of a `names` entry: the container
	// is run with `<flag>=<that entry's value>`.
	Args map[string]string `json:"args"`

	// What the two lists above refer to, filled in by Contract.link when the
	// contract is loaded.
	linked     bool
	attributes []ResourceAttribute
	args       map[string]string
}

// ResourceEnv is the environment variable the OpenTelemetry SDK reads resource
// attributes from, as comma-separated key=value pairs.
const ResourceEnv = "OTEL_RESOURCE_ATTRIBUTES"

// refs returns the ids of the entries of other sections the container refers
// to, sorted and without repeats.
func (c Container) refs() []string {
	out := slices.Clone(c.ResourceAttributes)
	for _, flag := range sortedKeys(c.Args) {
		out = append(out, c.Args[flag])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// refs returns the ids of the entries of other sections the render's
// containers refer to, sorted and without repeats.
func (r Render) refs() []string {
	var out []string
	for _, o := range r.Objects {
		for _, c := range o.Containers {
			out = append(out, c.refs()...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// eachContainer calls fn with every container under `charts`, and the id of
// the object it belongs to. fn may change the container.
func (c *Contract) eachContainer(fn func(object string, ct *Container)) {
	for _, r := range c.Charts {
		for _, o := range r.Objects {
			for i := range o.Containers {
				fn(o.ID, &o.Containers[i])
			}
		}
	}
}

// attributesByID and namesByID index the two sections a container refers to.
func (c *Contract) attributesByID() map[string]ResourceAttribute {
	out := map[string]ResourceAttribute{}
	for _, a := range c.ResourceAttributes {
		out[a.ID] = a
	}
	return out
}

func (c *Contract) namesByID() map[string]string {
	out := map[string]string{}
	for _, n := range c.Names {
		out[n.ID] = n.Value
	}
	return out
}

// validateLinks checks what the chart containers refer to: every id is an
// entry of the right section, and a resource attribute is referred to by the
// containers of exactly the components its entry lists. A referred-to entry
// is one the charts are held to, so `components` cannot say more, or less,
// than what is rendered and compared.
func (c *Contract) validateLinks() []string {
	var problems []string
	attributes, names := c.attributesByID(), c.namesByID()
	referrers := map[string][]string{}
	c.eachContainer(func(object string, ct *Container) {
		for _, id := range ct.ResourceAttributes {
			if _, ok := attributes[id]; !ok {
				problems = append(problems, fmt.Sprintf("%s: container %q refers to the resource attribute %q, and `resource_attributes` has no entry with that id", object, ct.Name, id))
				continue
			}
			referrers[id] = append(referrers[id], ct.Name)
		}
		problems = append(problems, ct.validateArgs(object, names)...)
	})
	for _, id := range sortedKeys(referrers) {
		got := slices.Compact(sorted(referrers[id]))
		want := sorted(attributes[id].Components)
		if !slices.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("%s lists the components [%s], and the chart containers that refer to it are [%s]: the container of each component (named after it) lists the id in its `resource_attributes` under `charts`, and no other container does",
				id, strings.Join(want, ", "), strings.Join(got, ", ")))
		}
	}
	return problems
}

func (c Container) validateArgs(object string, names map[string]string) []string {
	var problems []string
	for _, flag := range sortedKeys(c.Args) {
		if !strings.HasPrefix(flag, "-") || strings.Contains(flag, "=") {
			problems = append(problems, fmt.Sprintf("%s: container %q: the key %q under args is not a flag (write it as the container is run with it, like --mesh-domain, without a value)", object, c.Name, flag))
		}
		if _, ok := names[c.Args[flag]]; !ok {
			problems = append(problems, fmt.Sprintf("%s: container %q takes %s from %q, and `names` has no entry with that id", object, c.Name, flag, c.Args[flag]))
		}
	}
	return problems
}

// link resolves what each chart container refers to, so a render is compared
// with the entry itself. Called on a contract that validateLinks accepts.
func (c *Contract) link() {
	attributes, names := c.attributesByID(), c.namesByID()
	c.eachContainer(func(_ string, ct *Container) {
		ct.linked = true
		ct.attributes = nil
		for _, id := range ct.ResourceAttributes {
			ct.attributes = append(ct.attributes, attributes[id])
		}
		ct.args = map[string]string{}
		for flag, id := range ct.Args {
			ct.args[flag] = names[id]
		}
	})
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

// OwnedIDs compares the ids of renders, of their objects and of the entries of
// other sections their containers refer to (a resource attribute, the name an
// argument takes its value from) with the ids a chart test declares it holds
// (the `ids` of its helm_contract_test), in both directions, and returns what
// differs.
func OwnedIDs(renders []Render, ids []string) []string {
	var have, referred []string
	for _, r := range renders {
		have = append(have, r.ID)
		for _, o := range r.Objects {
			have = append(have, o.ID)
		}
		referred = append(referred, r.refs()...)
	}
	slices.Sort(referred)
	referred = slices.Compact(referred)
	var problems []string
	for _, id := range ids {
		if id != "" && !slices.Contains(have, id) && !slices.Contains(referred, id) {
			problems = append(problems, fmt.Sprintf("%s no longer has the chart entry %q (or no container of this chart's renders refers to an entry of that id any more), and the test still lists it in `ids`. "+
				"If a harness may no longer rely on it, bump `version` and remove the id from the test's `ids` in the same change; otherwise put the entry back.", File, id))
		}
	}
	for _, id := range have {
		if !slices.Contains(ids, id) {
			problems = append(problems, fmt.Sprintf("%s has the chart entry %q, and the test that renders its chart does not list it in `ids` (test/harnesscontract/BUILD.bazel): add it there.", File, id))
		}
	}
	for _, id := range referred {
		if !slices.Contains(ids, id) {
			problems = append(problems, fmt.Sprintf("%s holds a container of this chart to the entry %q, and the test that renders the chart does not list it in `ids` (test/harnesscontract/BUILD.bazel): add it there.", File, id))
		}
	}
	return problems
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
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
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
// labels, the names of environment variables, the keys of the resource
// attributes one sets, and the names of command-line flags: a chart can
// generate key material at render time.
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

func (got container) env(name string) string {
	value := ""
	for _, e := range got.Env {
		if e.Name == name {
			value = e.Value
		}
	}
	return value
}

func (c Container) checkEnv(what string, got container) []string {
	var problems []string
	for _, env := range sortedKeys(c.EnvContains) {
		value := got.env(env)
		for _, want := range c.EnvContains[env] {
			if !strings.Contains(value, want) {
				// Not the value: an environment variable can hold a secret.
				problems = append(problems, fmt.Sprintf("%s container %q: %s does not contain %q", what, c.Name, env, want))
			}
		}
	}
	if !c.linked && len(c.refs()) > 0 {
		// A contract that was not loaded through Load: comparing nothing must
		// not read as agreement.
		return append(problems, fmt.Sprintf("%s container %q refers to other entries (%s) and they were not resolved: load the contract with Load", what, c.Name, strings.Join(c.refs(), ", ")))
	}
	problems = append(problems, c.checkResourceAttributes(what, got)...)
	return append(problems, c.checkArgs(what, got)...)
}

// checkResourceAttributes holds the container's OTEL_RESOURCE_ATTRIBUTES to
// the `resource_attributes` entries it refers to: the entry's attribute is the
// key of one of the pairs, with the entry's value when it has one.
func (c Container) checkResourceAttributes(what string, got container) []string {
	if len(c.attributes) == 0 {
		return nil
	}
	pairs := map[string]string{}
	for pair := range strings.SplitSeq(got.env(ResourceEnv), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			pairs[k] = v
		}
	}
	var problems []string
	for _, a := range c.attributes {
		v, ok := pairs[a.Attribute]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s container %q: %s sets no resource attribute %q, which the entry %s says the component carries (it sets: %s)",
				what, c.Name, ResourceEnv, a.Attribute, a.ID, orNone(sortedKeys(pairs))))
		case a.Value != "" && v != a.Value:
			// Not the value it has: an environment variable can hold a secret.
			problems = append(problems, fmt.Sprintf("%s container %q: %s sets %s to something other than %q, the value of the entry %s", what, c.Name, ResourceEnv, a.Attribute, a.Value, a.ID))
		}
	}
	return problems
}

// checkArgs holds the container's command line to the `names` entries its
// `args` refer to: `<flag>=<value>` is one of the arguments.
func (c Container) checkArgs(what string, got container) []string {
	var problems []string
	line := slices.Concat(got.Command, got.Args)
	for _, flag := range sortedKeys(c.args) {
		want := flag + "=" + c.args[flag]
		if slices.Contains(line, want) {
			continue
		}
		// Not the value it has: an argument can hold a secret.
		has := "it has no " + flag + " argument"
		if slices.ContainsFunc(line, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") }) {
			has = "it has " + flag + " with another value"
		}
		problems = append(problems, fmt.Sprintf("%s container %q is not run with %s, the value of the entry %s (%s)", what, c.Name, want, c.Args[flag], has))
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
