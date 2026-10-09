package harnesscontract

import (
	"fmt"
	"regexp"
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
	CheckedBy CheckedBy         `json:"checked_by"`
}

// Object is one rendered object. Only what is set is checked.
type Object struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Name is the object's name, or NameFrom is the id of the `names` entry
	// whose value it is: an object the chart names after something the code
	// names too is held to that entry, not to a second copy of the string.
	Name          string            `json:"name"`
	NameFrom      string            `json:"name_from"`
	Namespace     string            `json:"namespace"`
	PodLabels     map[string]string `json:"pod_labels"`
	Containers    []Container       `json:"containers"`
	RollingUpdate *RollingUpdate    `json:"rolling_update"`
	// HostPaths are patterns, `<id>` standing for the value of a `names` entry:
	// the pod has a hostPath volume whose path ends with each.
	HostPaths []string `json:"host_paths"`
	// Webhooks maps the name of a webhook of an admission configuration to the
	// labels it selects by.
	Webhooks map[string]WebhookSelector `json:"webhooks"`

	// What the fields above refer to, filled in by Contract.link.
	linked    bool
	nameFrom  string
	hostPaths []string
	webhooks  map[string]WebhookSelector
}

// WebhookSelector says which label a webhook selects by, and with which of its
// two selectors: each is the id of a `names` entry (and, once linked, that
// entry's value), a key of the selector's matchLabels with the value "true".
// The two are not interchangeable: one selects the namespaces whose pods the
// webhook sees, the other the pods themselves.
type WebhookSelector struct {
	Namespaces string `json:"namespaces"`
	Objects    string `json:"objects"`
}

func (s WebhookSelector) refs() []string {
	var out []string
	for _, id := range []string{s.Namespaces, s.Objects} {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// refs returns the ids of the `names` entries the object itself refers to
// (its containers have their own), sorted and without repeats.
func (o Object) refs() []string {
	var out []string
	if o.NameFrom != "" {
		out = append(out, o.NameFrom)
	}
	for _, pattern := range o.HostPaths {
		out = append(out, argRefs(pattern)...)
	}
	for _, webhook := range sortedKeys(o.Webhooks) {
		out = append(out, o.Webhooks[webhook].refs()...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// name is the name the object is rendered under.
func (o Object) name() string {
	if o.NameFrom != "" {
		return o.nameFrom
	}
	return o.Name
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
	// CodeResourceAttributes are ids of `resource_attributes` entries the
	// component's own code sets. The OpenTelemetry SDK lets the environment
	// win over the code, so a chart that gave the container the attribute would
	// change what is deployed while the code still says what the entry says:
	// the container is given it neither in OTEL_RESOURCE_ATTRIBUTES nor, for
	// service.name, as OTEL_SERVICE_NAME.
	CodeResourceAttributes []string `json:"code_resource_attributes"`
	// Args maps a command-line flag to the id of a `names` entry: the container
	// is run with `<flag>=<that entry's value>`. A value with `<id>` in it is a
	// pattern instead: the container is run with the flag set to that text,
	// each `<id>` replaced by the entry's value (`127.0.0.1:<port.outbound_http>`).
	Args map[string]string `json:"args"`

	// What the lists above refer to, filled in by Contract.link when the
	// contract is loaded.
	linked         bool
	attributes     []ResourceAttribute
	codeAttributes []ResourceAttribute
	args           map[string]string
}

// ResourceEnv is the environment variable the OpenTelemetry SDK reads resource
// attributes from, as comma-separated key=value pairs. ServiceNameEnv is the
// one it reads service.name from, ahead of ResourceEnv.
const (
	ResourceEnv          = "OTEL_RESOURCE_ATTRIBUTES"
	ServiceNameEnv       = "OTEL_SERVICE_NAME"
	serviceNameAttribute = "service.name"
)

// argRef is an `<id>` in the value of an `args` pair.
var argRef = regexp.MustCompile(`<([^<>]*)>`)

// argRefs returns the ids of the `names` entries the value of an `args` pair
// refers to: the whole value, or every `<id>` in it.
func argRefs(value string) []string {
	matches := argRef.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return []string{value}
	}
	var out []string
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// argValue is the value the flag is run with, the ids resolved through names.
func argValue(value string, names map[string]string) string {
	if !argRef.MatchString(value) {
		return names[value]
	}
	return argRef.ReplaceAllStringFunc(value, func(ref string) string { return names[ref[1:len(ref)-1]] })
}

// refs returns the ids of the entries of other sections the container refers
// to, sorted and without repeats.
func (c Container) refs() []string {
	out := slices.Concat(c.ResourceAttributes, c.CodeResourceAttributes)
	for _, flag := range sortedKeys(c.Args) {
		out = append(out, argRefs(c.Args[flag])...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// refs returns the ids of the entries of other sections the render's objects
// and their containers refer to, sorted and without repeats.
func (r Render) refs() []string {
	var out []string
	for _, o := range r.Objects {
		out = append(out, o.refs()...)
		for _, c := range o.Containers {
			out = append(out, c.refs()...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// eachObject calls fn with every object under `charts`. fn may change the
// object.
func (c *Contract) eachObject(fn func(o *Object)) {
	for _, r := range c.Charts {
		for i := range r.Objects {
			fn(&r.Objects[i])
		}
	}
}

// eachContainer calls fn with every container under `charts`, and the id of
// the object it belongs to. fn may change the container.
func (c *Contract) eachContainer(fn func(object string, ct *Container)) {
	c.eachObject(func(o *Object) {
		for i := range o.Containers {
			fn(o.ID, &o.Containers[i])
		}
	})
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

// validateLinks checks what the chart objects and their containers refer to:
// every id is an entry of the right section, and a resource attribute is
// referred to by the containers of exactly the components its entry lists. A
// referred-to entry is one the charts are held to, so `components` cannot say
// more, or less, than what is rendered and compared.
func (c *Contract) validateLinks() []string {
	var problems []string
	attributes, names := c.attributesByID(), c.namesByID()
	c.eachObject(func(o *Object) { problems = append(problems, o.validateRefs(names)...) })
	referrers := map[string][]string{}
	c.eachContainer(func(object string, ct *Container) {
		problems = append(problems, ct.validateAttributes(object, attributes, referrers)...)
		problems = append(problems, ct.validateArgs(object, names)...)
	})
	for _, id := range sortedKeys(referrers) {
		got := slices.Compact(sorted(referrers[id]))
		want := sorted(attributes[id].Components)
		if !slices.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("%s lists the components [%s], and the chart containers that refer to it are [%s]: the container of each component (named after it) lists the id in its `resource_attributes` (or `code_resource_attributes`) under `charts`, and no other container does",
				id, strings.Join(want, ", "), strings.Join(got, ", ")))
		}
	}
	return append(problems, c.validateHolders()...)
}

// validateRefs checks the `names` entries the object itself refers to.
func (o Object) validateRefs(names map[string]string) []string {
	var problems []string
	if _, ok := names[o.NameFrom]; o.NameFrom != "" && !ok {
		problems = append(problems, fmt.Sprintf("%s takes its name from %q, and `names` has no entry with that id", o.ID, o.NameFrom))
	}
	for _, webhook := range sortedKeys(o.Webhooks) {
		if len(o.Webhooks[webhook].refs()) == 0 {
			problems = append(problems, fmt.Sprintf("%s: the webhook %q is held to nothing: give it `namespaces` or `objects`, the id of the `names` entry that selector selects by", o.ID, webhook))
		}
	}
	for _, id := range (Object{HostPaths: o.HostPaths, Webhooks: o.Webhooks}).refs() {
		if _, ok := names[id]; !ok {
			problems = append(problems, fmt.Sprintf("%s refers to %q under `host_paths` or `webhooks`, and `names` has no entry with that id (a host path is a pattern, like /plugins/<csi.driver>)", o.ID, id))
		}
	}
	return problems
}

// validateAttributes checks the resource attributes the container refers to,
// and adds the container to the referrers of each one the contract has.
func (c Container) validateAttributes(object string, attributes map[string]ResourceAttribute, referrers map[string][]string) []string {
	var problems []string
	for _, id := range slices.Concat(c.ResourceAttributes, c.CodeResourceAttributes) {
		if _, ok := attributes[id]; !ok {
			problems = append(problems, fmt.Sprintf("%s: container %q refers to the resource attribute %q, and `resource_attributes` has no entry with that id", object, c.Name, id))
			continue
		}
		referrers[id] = append(referrers[id], c.Name)
	}
	for _, id := range c.CodeResourceAttributes {
		if slices.Contains(c.ResourceAttributes, id) {
			problems = append(problems, fmt.Sprintf("%s: container %q lists %q under both `resource_attributes` (the chart gives it) and `code_resource_attributes` (the chart must not): it is one or the other", object, c.Name, id))
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
		for _, id := range argRefs(c.Args[flag]) {
			if _, ok := names[id]; !ok {
				problems = append(problems, fmt.Sprintf("%s: container %q takes %s from %q, and `names` has no entry with that id", object, c.Name, flag, id))
			}
		}
	}
	return problems
}

// chartTies returns, for every entry a render refers to, the chart tests that
// hold such a render (tied), and every chart test there is.
func (c *Contract) chartTies() (tied map[string][]string, chartTests []string) {
	tied = map[string][]string{}
	for _, r := range c.Charts {
		tests := r.CheckedBy.tests()
		chartTests = append(chartTests, tests...)
		for _, id := range r.refs() {
			tied[id] = append(tied[id], tests...)
		}
	}
	return tied, chartTests
}

// validateHolders checks that `checked_by` and the ties under `charts` say the
// same thing. An entry of another section that a render refers to is compared
// with that render by the render's chart test, so its checked_by names that
// test; and an entry whose checked_by names a chart test is referred to by a
// render that test holds, or the test would compare it with nothing.
func (c *Contract) validateHolders() []string {
	tied, chartTests := c.chartTies()
	var problems []string
	for _, e := range c.entries() {
		if e.section == chartsSection {
			continue
		}
		for _, test := range slices.Compact(sorted(tied[e.id])) {
			if !e.checkedBy.Has(test) {
				problems = append(problems, fmt.Sprintf("%s is compared with a render by %s (a render that test holds refers to it), and its checked_by does not name that test: add it, so the entry lists every test that holds it", e.id, test))
			}
		}
		for _, test := range e.checkedBy.tests() {
			if slices.Contains(chartTests, test) && !slices.Contains(tied[e.id], test) {
				problems = append(problems, fmt.Sprintf("%s: checked_by names the chart test %s, and no render that test holds refers to the entry (an object's `name_from`; a container's `resource_attributes`, `code_resource_attributes` or `args`): the test would compare it with nothing", e.id, test))
			}
		}
	}
	return problems
}

// link resolves what each chart object and container refers to, so a render is
// compared with the entry itself. Called on a contract that validateLinks
// accepts.
func (c *Contract) link() {
	attributes, names := c.attributesByID(), c.namesByID()
	c.eachObject(func(o *Object) {
		o.linked = true
		o.nameFrom = names[o.NameFrom]
		o.hostPaths = nil
		for _, pattern := range o.HostPaths {
			o.hostPaths = append(o.hostPaths, argValue(pattern, names))
		}
		o.webhooks = map[string]WebhookSelector{}
		for webhook, s := range o.Webhooks {
			o.webhooks[webhook] = WebhookSelector{Namespaces: names[s.Namespaces], Objects: names[s.Objects]}
		}
	})
	c.eachContainer(func(_ string, ct *Container) {
		ct.linked = true
		ct.attributes, ct.codeAttributes = nil, nil
		for _, id := range ct.ResourceAttributes {
			ct.attributes = append(ct.attributes, attributes[id])
		}
		for _, id := range ct.CodeResourceAttributes {
			ct.codeAttributes = append(ct.codeAttributes, attributes[id])
		}
		ct.args = map[string]string{}
		for flag, value := range ct.Args {
			ct.args[flag] = argValue(value, names)
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
		// With neither a name nor a name_from it is the one object of its kind
		// in the render, whatever it is called.
		if o.ID == "" || o.Kind == "" || (o.Name != "" && o.NameFrom != "") {
			problems = append(problems, fmt.Sprintf("%s: an object needs an id and a kind, and has a name or a name_from but not both (got id=%q kind=%q name=%q name_from=%q)", r.ID, o.ID, o.Kind, o.Name, o.NameFrom))
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

// HeldByChartTest returns what differs between the contract and the chart test
// target, which renders the chart called name and declares in `ids` what it
// holds. Every render of the chart names the test in its checked_by; every
// entry whose checked_by names the test is one this chart's renders give it
// something to compare; and the ids are the ones OwnedIDs expects.
func (c *Contract) HeldByChartTest(target, name string, ids []string) []string {
	renders := c.RendersOf(name)
	var problems, mine []string
	for _, r := range renders {
		mine = append(mine, r.ID)
		mine = append(mine, r.refs()...)
		if !r.CheckedBy.Has(target) {
			problems = append(problems, fmt.Sprintf("%s: %s is a render of the chart %q and its checked_by does not name %s, the test that renders that chart: name it, so the entry lists the test that holds it.", File, r.ID, name, target))
		}
	}
	for _, id := range c.IDs(target) {
		if !slices.Contains(mine, id) {
			problems = append(problems, fmt.Sprintf("%s names %s in the checked_by of %q, and that entry is neither a render of the chart %q nor referred to by one: the test has nothing to compare it with.", File, target, id, name))
		}
	}
	return append(problems, OwnedIDs(renders, ids)...)
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
			problems = append(problems, fmt.Sprintf("%s no longer has the chart entry %q (or nothing in this chart's renders refers to an entry of that id any more), and the test still lists it in `ids`. "+
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
			problems = append(problems, fmt.Sprintf("%s holds this chart to the entry %q, and the test that renders the chart does not list it in `ids` (test/harnesscontract/BUILD.bazel): add it there.", File, id))
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
				Volumes        []struct {
					Name     string `json:"name"`
					HostPath *struct {
						Path string `json:"path"`
					} `json:"hostPath"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	// An admission configuration's.
	Webhooks []struct {
		Name              string   `json:"name"`
		NamespaceSelector selector `json:"namespaceSelector"`
		ObjectSelector    selector `json:"objectSelector"`
	} `json:"webhooks"`
}

type selector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []any             `json:"matchExpressions"`
}

// describe lists what the selector requires, for a failure message.
func (s selector) describe() string {
	out := orNone(labelPairs(s.MatchLabels))
	if n := len(s.MatchExpressions); n > 0 {
		out += fmt.Sprintf(", and %d matchExpressions", n)
	}
	return out
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
	Env     []envVar `json:"env"`
	EnvFrom []any    `json:"envFrom"`
	Mounts  []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"volumeMounts"`
}

// envVar is one variable. One that has ValueFrom has a value this check cannot
// read.
type envVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	ValueFrom any    `json:"valueFrom"`
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
		if o.name() == "" || d.Metadata.Name == o.name() {
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
	if !o.linked && len(o.refs()) > 0 {
		// A contract that was not loaded through Load: see checkEnv.
		return []string{fmt.Sprintf("%s refers to other entries (%s) and they were not resolved: load the contract with Load", o.ID, strings.Join(o.refs(), ", "))}
	}
	what := fmt.Sprintf("%s: %s/%s", o.ID, o.Kind, o.name())
	switch {
	case o.NameFrom != "":
		what += " (named by the entry " + o.NameFrom + ")"
	case o.Name == "":
		what = fmt.Sprintf("%s: the render's one %s", o.ID, o.Kind)
	}
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
	problems = append(problems, o.checkHostPaths(what, d)...)
	return append(problems, o.checkWebhooks(what, d)...)
}

// checkHostPaths holds the pod's hostPath volumes to the patterns: the path of
// one of them ends with each, and a container of the pod mounts that volume at
// a path that ends the same way. A component that derives a directory from a
// name writes there in its own filesystem; the host sees it only through the
// mount.
func (o Object) checkHostPaths(what string, d manifest) []string {
	pod := d.Spec.Template.Spec
	var paths, problems []string
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			paths = append(paths, v.HostPath.Path)
		}
	}
	by, whose := o.mounters(pod.Containers)
	for i, want := range o.hostPaths {
		found := false
		for _, v := range pod.Volumes {
			if v.HostPath == nil || !strings.HasSuffix(v.HostPath.Path, want) {
				continue
			}
			found = true
			if at := mountedAt(by, v.Name); !slices.ContainsFunc(at, func(p string) bool { return strings.HasSuffix(p, want) }) {
				problems = append(problems, fmt.Sprintf("%s: no container mounts the hostPath volume %q (%s) at a path ending with %s, which is what the contract's %s comes to (%s mount it at: %s)",
					what, v.Name, v.HostPath.Path, want, o.HostPaths[i], whose, orNowhere(at)))
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s has no hostPath volume whose path ends with %s, which is what the contract's %s comes to (its hostPath volumes: %s)", what, want, o.HostPaths[i], orNone(paths)))
		}
	}
	return problems
}

// mounters returns the containers whose mounts count, and what to call them
// in a failure: the ones the contract lists for the object, because those are
// the processes it speaks of (a sidecar that mounts the directory does not put
// the component's socket in it), or every container when it lists none.
func (o Object) mounters(containers []container) ([]container, string) {
	if len(o.Containers) == 0 {
		return containers, "its containers"
	}
	var out []container
	var names []string
	for _, listed := range o.Containers {
		names = append(names, listed.Name)
		for _, c := range containers {
			if c.Name == listed.Name {
				out = append(out, c)
			}
		}
	}
	return out, "the containers the contract lists for the object, " + strings.Join(names, ", ") + ","
}

// mountedAt returns the paths the containers mount the volume at.
func mountedAt(containers []container, volume string) []string {
	var out []string
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Name == volume {
				out = append(out, m.MountPath)
			}
		}
	}
	return out
}

func orNowhere(paths []string) string {
	if len(paths) == 0 {
		return "nowhere"
	}
	return strings.Join(paths, ", ")
}

// selectedValue is the value a webhook selects a label with.
const selectedValue = "true"

// checkWebhooks holds the webhooks of an admission configuration to the label
// each selects by, in the selector the contract names.
func (o Object) checkWebhooks(what string, d manifest) []string {
	var problems []string
	for _, name := range sortedKeys(o.webhooks) {
		var names []string
		var namespaces, objects selector
		found := false
		for _, w := range d.Webhooks {
			names = append(names, w.Name)
			if w.Name == name {
				found = true
				namespaces, objects = w.NamespaceSelector, w.ObjectSelector
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s has no webhook %q (its webhooks: %s)", what, name, orNone(names)))
			continue
		}
		problems = append(problems, selects(what, name, "namespaceSelector", namespaces, o.webhooks[name].Namespaces, o.Webhooks[name].Namespaces)...)
		problems = append(problems, selects(what, name, "objectSelector", objects, o.webhooks[name].Objects, o.Webhooks[name].Objects)...)
	}
	return problems
}

// selects reports a selector that is not exactly the label with the value
// "true". The label opts in with that value, so the key with another value
// matches nothing the mesh manages; and Kubernetes requires everything a
// selector lists, so one more label or any expression narrows what the webhook
// sees, down to nothing if the two contradict.
//
// A webhook's two selectors are required together too, so the one the contract
// does not name (no id) selects by nothing.
func selects(what, webhook, kind string, got selector, label, id string) []string {
	if id == "" {
		if len(got.MatchLabels)+len(got.MatchExpressions) == 0 {
			return nil
		}
		return []string{fmt.Sprintf("%s: the webhook %q also selects with its %s (%s), and the contract holds it to its other selector alone: every further requirement narrows what the webhook sees",
			what, webhook, kind, got.describe())}
	}
	problem := ""
	switch {
	case got.MatchLabels[label] != selectedValue:
		problem = "does not select by"
	case len(got.MatchLabels) > 1 || len(got.MatchExpressions) > 0:
		problem = "selects by more than"
	default:
		return nil
	}
	return []string{fmt.Sprintf("%s: the %s of the webhook %q %s the label %s=%s, the value of the entry %s with %q (it selects by: %s)",
		what, kind, webhook, problem, label, selectedValue, id, selectedValue, got.describe())}
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
	problems = append(problems, c.checkCodeResourceAttributes(what, got)...)
	return append(problems, c.checkArgs(what, got)...)
}

// resourcePairs returns the resource attributes the container's
// OTEL_RESOURCE_ATTRIBUTES sets.
func (got container) resourcePairs() map[string]string {
	pairs := map[string]string{}
	for pair := range strings.SplitSeq(got.env(ResourceEnv), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			pairs[k] = v
		}
	}
	return pairs
}

// checkCodeResourceAttributes holds the container's environment to the
// `resource_attributes` entries the component's own code sets: the chart gives
// the container none of them, because the environment would win.
func (c Container) checkCodeResourceAttributes(what string, got container) []string {
	if len(c.codeAttributes) == 0 {
		return nil
	}
	var problems []string
	// What cannot be read cannot be shown not to set the attribute.
	if len(got.EnvFrom) > 0 {
		problems = append(problems, fmt.Sprintf("%s container %q takes variables from `envFrom`, which this check cannot read: one of them could be %s and replace what the component's own code sets", what, c.Name, ResourceEnv))
	}
	if slices.ContainsFunc(got.Env, func(e envVar) bool { return e.Name == ResourceEnv && e.ValueFrom != nil }) {
		problems = append(problems, fmt.Sprintf("%s container %q takes %s from `valueFrom`, which this check cannot read: it could set what the component's own code sets", what, c.Name, ResourceEnv))
	}
	pairs := got.resourcePairs()
	for _, a := range c.codeAttributes {
		if _, ok := pairs[a.Attribute]; ok {
			// Not the value it has: an environment variable can hold a secret.
			problems = append(problems, fmt.Sprintf("%s container %q: %s sets the resource attribute %q, which the entry %s says the component's own code sets. The environment wins over the code, so what is deployed is no longer what the code says",
				what, c.Name, ResourceEnv, a.Attribute, a.ID))
		}
		if a.Attribute == serviceNameAttribute && slices.ContainsFunc(got.Env, func(e envVar) bool { return e.Name == ServiceNameEnv }) {
			problems = append(problems, fmt.Sprintf("%s container %q is given %s, which replaces the %s the entry %s says the component's own code sets",
				what, c.Name, ServiceNameEnv, a.Attribute, a.ID))
		}
	}
	return problems
}

// checkResourceAttributes holds the container's OTEL_RESOURCE_ATTRIBUTES to
// the `resource_attributes` entries it refers to: the entry's attribute is the
// key of one of the pairs, with the entry's value when it has one.
func (c Container) checkResourceAttributes(what string, got container) []string {
	if len(c.attributes) == 0 {
		return nil
	}
	pairs := got.resourcePairs()
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
		from := "the value of the entry " + c.Args[flag]
		if argRef.MatchString(c.Args[flag]) {
			from = "which is what the contract's " + c.Args[flag] + " comes to"
		}
		problems = append(problems, fmt.Sprintf("%s container %q is not run with %s, %s (%s)", what, c.Name, want, from, has))
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
