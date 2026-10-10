# The external-harness contract

[`external-harness.yaml`](./external-harness.yaml) lists what a harness
outside this repository may read from a deployed mesh: metric names, their
labels and the closed sets of label values, the fields of a log line, an Envoy
stat prefix, the annotations and ports a workload manifest names, and the
chart objects a harness rolls or reads.

A harness that lives in this repository changes in the same pull request as
the code it reads. One that lives elsewhere does not, and the first sign of a
rename would be a query that returns nothing, which often reads exactly like
a healthy zero. The contract replaces that atomicity: the names are written
down once, tests hold the code to them, and the harness pins the contract
`version` it was written against.

## The rule

**Change the code and `external-harness.yaml` together, in one pull request,
and say so in its description.**

| Change | `version` |
|---|---|
| An entry is removed | bump |
| An entry changes meaning: a renamed metric, label, field or object; a label value removed from a closed set, **or added to one**; a field that changes type or unit; another rolling-update strategy; a metric that keeps its name and labels and counts something else, or is absent under other conditions | bump |
| A label added to a metric, or removed from it | bump |
| A render made with other `set` pairs, release or namespace | bump |
| A new entry, a new object of a render | no bump |
| A new field on a log line that keeps every field it had (add it to `fields`) | no bump |
| Something more said of an entry that was said of nothing before: a key it did not have, one more pod label or container of an object, one more thing a container is held to | no bump |
| `checked_by`, the order of a list, `notes` or a comment reworded without saying anything else | no bump |
| What an entry MEANS, changed in `notes`, a comment or the code's behaviour while every key stays as it was (what a gauge counts, when a series is absent) | bump, and no check asks for it: see below |

A new value in a closed set is a change of meaning because a harness branches
on the set: a grader that knows four `reason` values has to decide what a
fifth one is, and until it does it can only fail. A new label on a metric is
one for the same reason: a harness selects one series by its labels, and the
selector it wrote now matches several.

### The bump is a test

[`external-harness.lock.yaml`](./external-harness.lock.yaml) holds, for the
contract's current `version`, one line per promise: `<entry id> <key>` (and
one more word per member where the contract says the set is open: the fields
of a log line, an object's pod labels and containers, what a container is held
to) with a digest of what is promised there. `TestVersionBump` in
`//test/harnesscontract:harnesscontract_test` compares the two files:

- A line of the lock that the contract no longer has, or whose digest no
  longer matches, is a promise a harness written against this version relied
  on. The test fails, names the entry and the key, and asks for the bump. It
  does not print a digest: there is nothing to paste that makes it pass
  without the bump.
- With `version` bumped by one, the test prints the whole lock for the new
  version. Replace the file with it in the same change.
- A promise the contract makes that the lock does not hold is new. The test
  prints the lines to add to the lock; `version` stays.
- The lock is written exactly as the test prints it: the header, then one
  promise to a line, sorted. A lock that holds the right promises in another
  shape fails, and the test prints the whole file. The comparison with the
  base (below) reads the lock line by line, so its form is part of the check.

That test reads both files from one checkout, so on its own it cannot see a
change made to both: an entry removed together with its lines, or a promise
changed and its digest computed by hand. (A line deleted alone does not pass:
the test asks for it back.) `scripts/check-harness-contract-bump.sh` closes
that. The `chart-version-bump` job of `ci.yaml` runs it on every pull request
with the base commit: every promise line of the lock at the base is still in
the lock, unchanged, or `version` is higher than at the base.

So the table above is what the two checks decide, with one limit, and it is a
real one. The lock holds the keys of an entry: names, types, sets, fields.
What an entry means beyond its keys is written in `notes` and comments (what
a gauge counts, when a series is absent and when it is zero, the entries
under "What is review-only"), and prose has no line. A change of meaning that
leaves every key alone, made in the code's behaviour and described in `notes`,
passes both checks with `version` unchanged. Version 2 was such a change: a
gauge kept its name, type and labels and counts something else. It is still a
bump: whoever changes what an entry means bumps `version` by hand, and whoever
reviews a diff of `notes` asks whether the meaning moved. A bump made for that
reason goes through the same steps as any other (the test prints the new
lock).

Every bump, whichever way it was found, gets a row in "Versions" below: that
table is the one place that says what a version changed and what a harness
pinned to an earlier one has to do.

## Versions

What each bump changed, for whoever maintains a harness pinned to an earlier
one.

| `version` | Change | What a harness has to change |
|---|---|---|
| 2 | `agent.xds_acked_tls_clusters` keeps its name, type and labels and means something else (#1508). It was the counts of the last snapshot whose cluster update the proxy acknowledged. It is the count of the clusters the proxy has accepted, each at the version it last acknowledged or stated. | Do not read it as "the proxy holds the newest snapshot" after any cluster ACK: a rejected cluster stays at its accepted version through later ACKs, so the gauge can stay above `agent.snapshot_tls_clusters` for as long as the proxy rejects. Treat an absent series as "not known", and expect it to become absent AFTER having had samples, while the proxy holds a cluster whose pin state the agent cannot determine: a grader that takes a missing series for zero, or that carries the last sample forward, is wrong there. The entry's `notes` have the full statement. |
| 1 | The first version (#1535). | |

## How an entry is tied to the code

Every entry has a `checked_by`: the Bazel test that compares it with the code
that produces the thing, a list of them when several tests hold it, or
`review-only`. Each test an entry names lists the entry among the ids it holds
(`Contract.Owns` in a Go test, `ids` of a `helm_contract_test`) and fails when
it does not, so `checked_by` is every test that fails when the entry and the
code part. For a chart test the contract does not even load unless the two
agree: an entry a render refers to names that render's test, and an entry
that names a chart test is referred to by one of its renders.

| Entries | Compared with | Test |
|---|---|---|
| `metrics` of the agent | The real instruments, collected after every series was recorded: registered name, type, attribute keys, and the values of each closed label in both directions | `//agent/internal/xds/cache/cachemetrics:cachemetrics_test` |
| `metrics` of the prober, `resource_attributes` `service.name` | Every `tier*` and `result*` string constant of `prober.go` (read from the source, so a new one cannot be forgotten) recorded through the real `record()`; the resource the prober builds | `//prober/internal/prober:prober_test` |
| `log_lines` | A burst written by the real fail log with the real cap and window, then flushed as a stopping prober does; the lines are parsed as a harness parses them (marker, one JSON object, exact keys in order, RFC 3339 times, the summary's `t` and `window_start`) | `//prober/internal/prober:prober_test` |
| `envoy_stats` | The stat prefix of the portless chain in a capture listener the real generator built | `//agent/internal/xds/proxy:proxy_test` |
| `names` | The Go constant the product itself uses | `//test/harnesscontract:harnesscontract_test`, and `//agent/internal/xds/xdsconst:xdsconst_test` for the one constant that is internal to the agent |
| `charts` | `helm template` of the packaged chart, run with the release name, namespace and `--set` pairs the entry gives | `//test/harnesscontract:{aether,prober,udsecho}_chart_test` |
| `resource_attributes` `k8s.node.name` | The keys `OTEL_RESOURCE_ATTRIBUTES` sets in the rendered agent and prober containers. The chart entries do not repeat the attribute: each container lists the entry's id (`resource_attributes`), and the chart test reads the key from the entry | `//test/harnesscontract:harnesscontract_test` holds the link (every component has a container that refers to the entry, and no other container does); `//test/harnesscontract:{aether,prober}_chart_test` compare with the render |
| `names` `mesh.default_domain` | Besides the Go constant: the `--mesh-domain` argument the aether chart renders for the agent and the mesh-DNS daemon when `meshDomain` is left at the chart's default, and the one the prober chart renders when `probe.meshDomain` is left at its default (a container's `args` maps the flag to the entry's id) | `//test/harnesscontract:{aether,prober}_chart_test`, with `harnesscontract_test` holding the link for the agent |
| `names` `port.outbound_http` | Besides the Go constant: the `--egress` argument the prober chart renders when `probe.egress` is left at its default (`args` maps the flag to a pattern, `127.0.0.1:<port.outbound_http>`) | `//test/harnesscontract:prober_chart_test` |
| `names` `csi.driver` | Besides the Go constant: the name of the CSIDriver object the aether chart renders (the object has `name_from: csi.driver` in place of a name), and the kubelet plugin directory of the uds-csi DaemonSet: a hostPath volume of the path `<--kubelet-root of the uds-csi container>/plugins/<csi.driver>`, mounted by that container at that same path, which is where the plugin writes its socket and where the kubelet looks (`host_paths: ["<arg:--kubelet-root>/plugins/<csi.driver>"]`; a pattern without `<arg:...>` holds only the end of the path) | `//test/harnesscontract:aether_chart_test` |
| `names` `pod.label.managed` | Besides the Go constant: the label the controller's two pod webhooks select by in the aether chart's render (`webhooks` of the render's one MutatingWebhookConfiguration says, for each webhook, which of its two selectors selects by the entry: that selector is exactly the one label with the value "true", with no other label and no expression, and the webhook's other selector is empty, since every further requirement narrows what the webhook sees). Both webhooks ignore failures, so another key there would fail silently | `//test/harnesscontract:aether_chart_test` |
| `resource_attributes` `service.name` of the prober | Besides the resource the prober builds: the rendered prober container is given neither `service.name` in `OTEL_RESOURCE_ATTRIBUTES` nor `OTEL_SERVICE_NAME`, because the environment can replace what the code sets. `OTEL_SERVICE_NAME` is an explicit override and always does, so that rule is the one that matters; `service.name` in `OTEL_RESOURCE_ATTRIBUTES` no longer renames the prober (#1562, held by the prober's own test), and is refused here as well so that a chart never says a name the component ignores. A container whose `OTEL_RESOURCE_ATTRIBUTES` comes from `valueFrom`, or that has any `envFrom`, fails too: what cannot be read could carry either (the container lists the entry in `code_resource_attributes`) | `//test/harnesscontract:prober_chart_test` |

The last five rows exist because a Go constant is not always what is
deployed. A component can be given the name by its chart (a flag, an
environment variable), the chart can write the name a second time (an object
it renders), or the chart's own defaults can spell it. Comparing the entry
with the constant alone then passes while a default install does something
else. When you add an entry, ask which of these holds; when one does, tie the
entry to the render as well.

A tie is a reference under `charts` to another entry: `name_from`, `args`,
`resource_attributes`, `code_resource_attributes`, `host_paths`, `webhooks`.
It is not a promise to a harness (an object's name is the same whether the
contract writes it out or takes it from an entry), so the lock does not hold
it, and writing the literal in its place changes nothing today. It would
change the next rename: the entry moves, the chart stays, and no test
compares the two any more. `TestTies` in `harnesscontract_test` therefore
holds the contract to the list of its ties (`ties` in `contract_test.go`), in
both directions: a tie that is gone fails, and a new one is added to the list.
Neither needs a version bump.

The lock is written to match. Where a promise is made through a tie, its line
is named and digested by what the tie comes to, never by the id of the entry:
an object's name, the value an argument is run with, the path a host path
pattern comes to, the attribute a container is given or is not to be given,
the label a webhook selects by. So a tie moved to another entry that says the
same is no bump (only `ties` changes), and the same tie coming to something
else is one.

`stored_name` is checked against the usual OTLP-to-Prometheus translation of
`otel_name`, computed by the test. Whether a given pipeline applies that
translation is not something this repository can test.

Run them all:

```bash
bazel test //test/harnesscontract:checks
```

`//test/harnesscontract:harnesscontract_test` also fails when a `checked_by`
names a test that is not in that suite, when a promise changed and `version`
did not (above), and refuses a contract that is not well formed (an unknown
key, an id used twice, an entry without `checked_by`, a `reason` that is in no
class or in two, a chart test named by an entry it compares with nothing).

### What is review-only

- **What a pipeline does to a name.** The `node` and `job` labels of a stored
  series come from OpenTelemetry resource attributes (`k8s.node.name`,
  `service.name`) through the metrics pipeline's own configuration. That the
  product sets the attribute is checked (the table above); the label it
  becomes is the pipeline's.
  The same holds for the stored name of an Envoy stat: only the prefix the
  agent chooses is tied to code.
- **The meaning of the two halves of `reason`** (`reason_classes`): under
  which reasons the node published no TLS at all, and under which it published
  TLS that checks no server identity. The code states it in comments only.
- **The name of a generated mesh Service** (the service's name, in the
  service's namespace). Only its label is tied to a constant.
- **A promise made only in prose.** `notes` and comments have no line in the
  lock, so the `version` bump for a change of meaning written only there is
  kept by review.
- **The names the prober and udsecho charts put on their own pods.** The mesh
  label, the annotations and the CSI driver of a volume are literals in those
  templates, used as any workload uses them. The contract compares the names
  with the Go constants, not with these copies; a copy that is wrong leaves
  that chart's own pods outside the mesh, which its end-to-end test sees.

## When a contract test fails

The message names this file and the rule. Then:

1. You renamed or removed something on purpose. Update the entry, bump
   `version` if the table above says so, and say in the pull request
   description that the contract changed, so whoever maintains a harness can
   find the change.
2. You did not mean to change it. The test just told you a harness reads it.

## Adding an entry

Add it to the YAML with a `checked_by`, then make that test know it: each
owning test declares the ids it holds (`Contract.Owns`), and fails when the
contract assigns it an entry it has no check for. Prefer comparing with what
the code really emits (an instrument's collected series, a line the real
formatter wrote, a render) over comparing two constants. Then run
`//test/harnesscontract:harnesscontract_test`: it prints the lines the new
entry adds to `external-harness.lock.yaml`.

A metric label is on every series unless it has `when`: the test looks at each
series on its own, because a harness selects one series by its labels.

For a chart object, write the YAML and add its id to the `ids` of that chart's
`helm_contract_test` in `BUILD.bazel`: the render options and the expectations
are both read from the entry, and `ids` is the test's list of what it holds
(it fails when the two differ, in either direction). When a container must be
given something another entry already names (a resource attribute, a name
passed as a flag), or an object is named by one, refer to that entry by id
(`resource_attributes`, `args`, `name_from`) instead of writing the string
again, add the id to `ids` as well, and add the chart test to that entry's
`checked_by`. Mind which namespace a
chart puts its objects in: `udsecho` takes it from its `namespace` value, not
from the release. The chart tests live here
and not in `charts/<chart>/BUILD.bazel` on purpose: a change under `charts/`
needs a chart version bump, and adding a contract entry is not a chart change.

## Removing an entry

Removing an entry while the code still emits the thing fails the owning test
(`... no longer has the entry "<id>" ..., and the code it describes is still
here`), and `TestVersionBump` fails whether the code still emits it or not
(`... no longer has the entry "<id>", which it promised at version N`). Remove
the check in the same change, bump `version`, and replace the lock with the
one the test then prints: the diff shows the entry, its check, the version and
the lock moving together. For a `charts` entry or one of its objects the check
is its id in the test's `ids`.

## What is not in the contract

The last section of the file lists names a harness uses that belong to its own
workloads and tools (a load generator's failure lines and counters, its client
loops' log lines, its own labels). The product does not emit them.
