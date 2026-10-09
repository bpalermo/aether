package harnesscontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// LockFile is the path in the repository of the contract's lock: the promises
// of the contract's current `version`, one line each.
const LockFile = "test/harnesscontract/external-harness.lock.yaml"

// Lock is external-harness.lock.yaml: for the version it was written for, the
// name of every promise the contract made and a digest of what was promised.
type Lock struct {
	Version  int               `json:"version"`
	Promises map[string]string `json:"promises"`
}

// ParseLock reads a lock. An unknown key or a promise listed twice is an
// error.
func ParseLock(data []byte) (*Lock, error) {
	l := &Lock{}
	if err := yaml.UnmarshalStrict(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", LockFile, err)
	}
	return l, nil
}

// notPromised are the keys of an entry that promise a harness nothing: its id
// is the name of the promise and not a part of it, `notes` is prose, and
// `checked_by` says who holds the entry, not what it says. Every other key is
// a promise, including one added to an entry type after this was written.
var notPromised = []string{"id", "notes", "checked_by"}

// promises is what a contract promises: the name of each promise (`<entry id>
// <key>`, and one more word per member where the key holds an open set) and
// what is promised under it, as canonical JSON.
type promises map[string]string

func (p promises) put(value any, path ...string) {
	b, err := json.Marshal(value)
	if err != nil {
		// Only values decoded from JSON, and strings, get here.
		panic(fmt.Sprintf("%s: %v", strings.Join(path, " "), err))
	}
	p[strings.Join(path, " ")] = string(b)
}

// rest promises every key left in fields that is set, each as one value: a
// change to any part of it is a change of the promise.
func (p promises) rest(fields map[string]any, path ...string) {
	for _, k := range sortedKeys(fields) {
		if slices.Contains(notPromised, k) || unset(fields[k]) {
			continue
		}
		p.put(fields[k], append(slices.Clone(path), k)...)
	}
}

// members promises each member of an open set on its own, so that a member the
// contract gains is a new promise and the others stand.
func (p promises) members(set []string, path ...string) {
	for _, member := range set {
		p.put(true, append(slices.Clone(path), member)...)
	}
}

// unset reports whether a decoded value is one the contract did not write.
func unset(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case bool:
		return !v
	case float64:
		return v == 0
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// fieldsOf returns an entry as the keys the contract file spells.
func fieldsOf(entry any) map[string]any {
	b, err := json.Marshal(entry)
	if err != nil {
		panic(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		panic(err)
	}
	return fields
}

// take removes key from fields.
func take(fields map[string]any, keys ...string) {
	for _, k := range keys {
		delete(fields, k)
	}
}

// Promises returns everything the contract promises a harness.
//
// The unit is chosen so that what README.md calls compatible adds promises and
// changes none, and everything else changes or removes one:
//
//   - a key of an entry is one promise, whatever its value: a closed set of
//     label values, a metric's labels, the classes of a reason set, the `set`
//     pairs of a render. Adding to such a value changes it;
//   - where the contract itself says the set is open, each member is its own
//     promise: the fields of a log line (with whether the field is a time),
//     the components of a resource attribute, an object's pod labels, its
//     containers, and what each container is held to;
//   - the order of a list is not a promise.
func (c *Contract) Promises() map[string]string {
	p := promises{}
	for _, m := range c.Metrics {
		m.promises(p)
	}
	for _, r := range c.ReasonClasses {
		r.promises(p)
	}
	for _, r := range c.ResourceAttributes {
		fields := fieldsOf(r)
		take(fields, "components")
		p.members(r.Components, r.ID, "components")
		p.rest(fields, r.ID)
	}
	for _, l := range c.LogLines {
		l.promises(p)
	}
	for _, s := range c.EnvoyStats {
		p.rest(fieldsOf(s), s.ID)
	}
	for _, n := range c.Names {
		p.rest(fieldsOf(n), n.ID)
	}
	for _, r := range c.Charts {
		r.promises(p)
	}
	return p
}

func (m Metric) promises(p promises) {
	fields := fieldsOf(m)
	labels := slices.Clone(m.Labels)
	for i := range labels {
		labels[i].Values = sorted(labels[i].Values)
	}
	slices.SortFunc(labels, func(a, b Label) int { return strings.Compare(a.Name, b.Name) })
	fields["labels"] = fieldsOf(struct {
		Labels []Label `json:"labels"`
	}{labels})["labels"]
	p.rest(fields, m.ID)
}

func (r ReasonClasses) promises(p promises) {
	fields := fieldsOf(r)
	classes := map[string]any{}
	for class, values := range r.Classes {
		classes[class] = sorted(values)
	}
	fields["classes"] = classes
	p.rest(fields, r.ID)
}

func (l LogLine) promises(p promises) {
	fields := fieldsOf(l)
	take(fields, "fields", "time_fields")
	for _, f := range l.Fields {
		kind := "any"
		if slices.Contains(l.TimeFields, f) {
			kind = "time"
		}
		p.put(kind, l.ID, "fields", f)
	}
	p.rest(fields, l.ID)
}

func (r Render) promises(p promises) {
	fields := fieldsOf(r)
	take(fields, "objects", "set")
	// The pairs a render is made with say under which install its objects are
	// what the contract says: always one promise, an empty one included.
	set := map[string]string{}
	for k, v := range r.Set {
		set[k] = v
	}
	p.put(set, r.ID, "set")
	p.rest(fields, r.ID)
	for _, o := range r.Objects {
		fields := fieldsOf(o)
		take(fields, "pod_labels", "containers", "name", "name_from")
		// The same object under another render is another promise.
		p.put(r.ID, o.ID, "render")
		// The name a harness addresses, wherever the contract takes it from.
		p.put(o.name(), o.ID, "name")
		for _, k := range sortedKeys(o.PodLabels) {
			p.put(o.PodLabels[k], o.ID, "pod_labels", k)
		}
		for _, ct := range o.Containers {
			ct.promises(p, o.ID, "containers", ct.Name)
		}
		p.rest(fields, o.ID)
	}
}

func (c Container) promises(p promises, path ...string) {
	at := func(more ...string) []string { return append(slices.Clone(path), more...) }
	fields := fieldsOf(c)
	take(fields, "name", "env_contains", "resource_attributes", "code_resource_attributes", "args")
	// That the pod has a container of this name.
	p.put(true, path...)
	for _, env := range sortedKeys(c.EnvContains) {
		p.members(c.EnvContains[env], at("env_contains", env)...)
	}
	p.members(c.ResourceAttributes, at("resource_attributes")...)
	p.members(c.CodeResourceAttributes, at("code_resource_attributes")...)
	for _, flag := range sortedKeys(c.Args) {
		// The value the container is run with, not the id it comes from.
		p.put(c.args[flag], at("args", flag)...)
	}
	p.rest(fields, path...)
}

// digests returns the promises as the lock holds them: by name, a digest of
// what is promised.
func (c *Contract) digests() map[string]string {
	out := map[string]string{}
	for name, promise := range c.Promises() {
		sum := sha256.Sum256([]byte(name + "\n" + promise))
		out[name] = hex.EncodeToString(sum[:8])
	}
	return out
}

func lockLine(name, digest string) string {
	return fmt.Sprintf("  %q: %q", name, digest)
}

// Lock returns the lock of the contract as it is now: what
// external-harness.lock.yaml holds once a version bump is recorded.
func (c *Contract) Lock() []byte {
	var b strings.Builder
	b.WriteString(lockHeader)
	fmt.Fprintf(&b, "version: %d\npromises:\n", c.Version)
	digests := c.digests()
	for _, name := range sortedKeys(digests) {
		b.WriteString(lockLine(name, digests[name]) + "\n")
	}
	return []byte(b.String())
}

const lockHeader = `# The promises external-harness.yaml made at the version below, one per line:
# "<entry id> <key> [<member>]" and a digest of what is promised there.
#
# //test/harnesscontract:harnesscontract_test compares this file with the
# contract. A line that is gone from the contract, or whose digest no longer
# matches, is a promise broken: the test fails until the contract's version is
# bumped, and then prints this whole file for the new version. A promise the
# contract gains needs no bump: the test prints the line to add here.
#
# So a line is added to this file whenever the contract gains a promise, and a
# line leaves it or changes only in the change that bumps the version. Never
# edit a digest or delete a line by hand to make the test pass: that is the
# bump the test asked for, skipped. README.md, "The rule".
`

// entryOf returns the entry id a promise's name starts with.
func entryOf(name string) string {
	id, _, _ := strings.Cut(name, " ")
	return id
}

// CheckLock compares the contract with the lock and returns what differs.
//
// At the lock's version, every promise in the lock is still made and still
// says the same (anything else needs a version bump), and every promise the
// contract makes is in the lock (a new one is added to it, with no bump). One
// version on, the lock is replaced as a whole.
func (c *Contract) CheckLock(lock *Lock) []string {
	switch {
	case lock.Version > c.Version:
		return []string{fmt.Sprintf("%s is at version %d and %s was written for version %d: a contract's version never goes back.", File, c.Version, LockFile, lock.Version)}
	case lock.Version < c.Version:
		var problems []string
		if c.Version != lock.Version+1 {
			problems = append(problems, fmt.Sprintf("%s went from version %d to %d: a bump is by one.", File, lock.Version, c.Version))
		}
		return append(problems, fmt.Sprintf("%s is at version %d and %s still holds the promises of version %d. Replace that file with this, in the change that bumps the version:\n\n%s",
			File, c.Version, LockFile, lock.Version, c.Lock()))
	}
	now := c.digests()
	entries := map[string]bool{}
	for name := range now {
		entries[entryOf(name)] = true
	}
	broken := map[string][]string{}
	for _, name := range sortedKeys(lock.Promises) {
		id, what, _ := strings.Cut(name, " ")
		digest, made := now[name]
		switch {
		case !made:
			broken[id] = append(broken[id], what+" is gone")
		case digest != lock.Promises[name]:
			broken[id] = append(broken[id], what+" changed")
		}
	}
	var added []string
	for _, name := range sortedKeys(now) {
		if _, locked := lock.Promises[name]; !locked {
			added = append(added, lockLine(name, now[name]))
		}
	}
	var problems []string
	for _, id := range sortedKeys(broken) {
		if !entries[id] {
			problems = append(problems, fmt.Sprintf("%s no longer has the entry %q, which it promised at version %d (a renamed entry is a removed one).", File, id, c.Version))
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: the entry %q no longer promises what it did at version %d: %s.", File, id, c.Version, strings.Join(broken[id], "; ")))
	}
	if len(problems) > 0 {
		return append(problems, fmt.Sprintf("A harness written against version %d breaks on each of these. If the change is meant, bump `version` to %d in %s in the same change; this test then prints %s for the new version. If it is not, undo it.",
			c.Version, c.Version+1, File, LockFile))
	}
	if len(added) > 0 {
		return []string{fmt.Sprintf("%s promises %d things that %s does not hold yet. A new promise needs no version bump: add these lines under `promises` there, so that a later change to them is seen.\n\n%s\n",
			File, len(added), LockFile, strings.Join(added, "\n"))}
	}
	return nil
}
