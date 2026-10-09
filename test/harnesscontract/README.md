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
| An entry changes meaning: a renamed metric, label, field or object; a label value removed from a closed set, **or added to one**; a field that changes type or unit; another rolling-update strategy | bump |
| A new entry | no bump |
| A new field on a log line that keeps every field it had (add it to `fields`) | no bump |

A new value in a closed set is a change of meaning because a harness branches
on the set: a grader that knows four `reason` values has to decide what a
fifth one is, and until it does it can only fail.

The bump itself is a review rule: no test in this repository can compare the
file with its previous revision. What the tests do guarantee is that the
contract cannot drift from the code without a test failing, and that an entry
cannot be removed silently (see "Removing an entry").

## How an entry is tied to the code

Every entry has a `checked_by`: the Bazel test that compares it with the code
that produces the thing, or `review-only`.

| Entries | Compared with | Test |
|---|---|---|
| `metrics` of the agent | The real instruments, collected after every series was recorded: registered name, type, attribute keys, and the values of each closed label in both directions | `//agent/internal/xds/cache/cachemetrics:cachemetrics_test` |
| `metrics` of the prober, `resource_attributes` `service.name` | Every `tier*` and `result*` string constant of `prober.go` (read from the source, so a new one cannot be forgotten) recorded through the real `record()`; the resource the prober builds | `//prober/internal/prober:prober_test` |
| `log_lines` | A burst written by the real fail log with the real cap and window, then flushed as a stopping prober does; the lines are parsed as a harness parses them (marker, one JSON object, exact keys in order, RFC 3339 times, the summary's `t` and `window_start`) | `//prober/internal/prober:prober_test` |
| `envoy_stats` | The stat prefix of the portless chain in a capture listener the real generator built | `//agent/internal/xds/proxy:proxy_test` |
| `names` | The Go constant the product itself uses | `//test/harnesscontract:harnesscontract_test`, and `//agent/internal/xds/xdsconst:xdsconst_test` for the one constant that is internal to the agent |
| `charts` | `helm template` of the packaged chart, run with the release name, namespace and `--set` pairs the entry gives | `//test/harnesscontract:{aether,prober,udsecho}_chart_test` |

`stored_name` is checked against the usual OTLP-to-Prometheus translation of
`otel_name`, computed by the test. Whether a given pipeline applies that
translation is not something this repository can test.

Run them all:

```bash
bazel test //test/harnesscontract:checks
```

`//test/harnesscontract:harnesscontract_test` also fails when a `checked_by`
names a test that is not in that suite, and refuses a contract that is not
well formed (an unknown key, an id used twice, an entry without `checked_by`,
a `reason` that is in no class or in two).

### What is review-only

- **What a pipeline does to a name.** The `node` and `job` labels of a stored
  series come from OpenTelemetry resource attributes (`k8s.node.name`,
  `service.name`) through the metrics pipeline's own configuration. The charts
  are checked to pass `k8s.node.name`; the label it becomes is the pipeline's.
  The same holds for the stored name of an Envoy stat: only the prefix the
  agent chooses is tied to code.
- **The meaning of the two halves of `reason`** (`reason_classes`): under
  which reasons the node published no TLS at all, and under which it published
  TLS that checks no server identity. The code states it in comments only.
- **The name of a generated mesh Service** (the service's name, in the
  service's namespace). Only its label is tied to a constant.
- **A single object removed from a `charts` entry.** The chart tests check the
  objects that are listed; nothing knows an object used to be listed.
- **The `version` bump.**

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
formatter wrote, a render) over comparing two constants.

For a chart object there is nothing to write but the YAML: the render options
and the expectations are both read from the entry. The chart tests live here
and not in `charts/<chart>/BUILD.bazel` on purpose: a change under `charts/`
needs a chart version bump, and adding a contract entry is not a chart change.

## Removing an entry

Removing an entry while the code still emits the thing fails the owning test
(`... no longer has the entry "<id>" ..., and the code it describes is still
here`). Remove the check in the same change and bump `version`: the diff then
shows the entry, its check and the version moving together.

## What is not in the contract

The last section of the file lists names a harness uses that belong to its own
workloads and tools (a load generator's failure lines and counters, its client
loops' log lines, its own labels). The product does not emit them.
