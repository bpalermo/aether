# e2e scripts

Kind harnesses for the mesh. Each script documents its own legs, knobs and
gates in its header. The long-running soak and the collector-pressure test (the
on-demand regression test for #662: a collector that refuses telemetry must not
block or kill an agent's start) are run by an external harness, maintained
outside this repository.

So are six kind reproductions of fixed incidents that no workflow here ran:
`agent-restart-gap.sh` (#1123, proposal 041), `drain-propagation.sh` (#1103,
#1124), `eastwest-quic-deadpeer.sh` (#1087, #1104), `eastwest-quic-hotrestart.sh`
(#1009, #1054), `hotrestart-wedge.sh` (#1050) and `proxy-concurrency-change.sh`
(#1136). They left this directory on 2026-10-10 and are maintained with that
external harness, where they install the published chart instead of building
this tree. The runbook still names each beside the incident it reproduces.

What is here is what something runs: the suites of the nightly `e2e` workflow
(`multicluster_waypoint.sh`, `multicluster_replicator.sh`, `uds.sh`,
`uds-csi.sh`, `authz.sh`, `l4routes.sh`, `eastwest-quic.sh`,
`first-install.sh`), the version pins they source with their consistency
tests, and two scripts run by hand: `multicluster_config.sh` (the runbook's
"Local multi-cluster end-to-end") and `multiprotocol.sh`.

Every harness that creates a kind cluster sources
[`kind-version.sh`](kind-version.sh) and passes `--image "$KIND_NODE_IMAGE"`, so
a local run and CI run the same Kubernetes; bumping it is one file
([runbook](../docs/runbook.md#bumping-the-e2e-kubernetes-version)).
`//e2e:kind_pin_test` enforces both.

Two more pins live beside it, each with its test and its runbook section:
[`gateway-api-version.sh`](gateway-api-version.sh), the Gateway API release whose
CRDs a harness installs (source it; never assign `GWAPI_VERSION` in a script,
`//e2e:gateway_api_pin_test`), and [`helm-version.sh`](helm-version.sh), the Helm
releases CI installs (`//e2e:helm_pin_test`). A harness runs whatever `helm` is
first on `PATH`, so write it for Helm 3 and Helm 4 alike: `helm list -a` exists
only in Helm 3, and `helm_list_all_flags` gives the flags for the one in use.

## Rules for script authors

### No early-exit reader in a pipeline (SIGPIPE under `pipefail`)

Every script here runs under `set -euo pipefail`. A pipeline whose **reader exits before its writer is
done** kills the writer with SIGPIPE, and `pipefail` turns that into a pipeline
status of **141**. Under `set -e` the script then stops dead, with no failed
assertion and no message, at a point that depends on timing and on how much the
writer had left to print (#1121: `eastwest-quic.sh verify` died this way at
random points). Inside an `if`/`until`/`&&` it is worse: the condition silently
reads as false.

Readers that exit early, and their read-to-EOF replacements:

| Don't                              | Do                                                      |
| ---------------------------------- | ------------------------------------------------------- |
| `… \| head -n 1` / `head -1`        | `… \| sed -n '1p'`                                       |
| `… \| head -40`                    | `… \| sed -n '1,40p'`                                    |
| `… \| head -40 \| sed 's/^/  /'`    | `… \| sed -n '1,40s/^/  /p'`                             |
| `… \| grep -q PAT`                 | `… \| grep -c PAT >/dev/null` (same exit status)        |
| `… \| grep -m1 PAT`                | `… \| grep PAT \| sed -n '1p'`                           |
| `… \| awk 'COND { print $1; exit }'` | `… \| awk '!f && COND { print $1; f = 1 }'`             |
| `… \| sed '…;q'`                   | `sed -n` with an address range and no `q`               |

When the writer is a `kubectl` or `curl` (or an Envoy admin dump) whose output
can be large, capture it first and test the variable — a here-string has no
writer process to kill:

```bash
grep -q "inbound_${ns}_${pod}" <<<"$(admin /listeners 2>/dev/null)"
```

`grep -q`, `head` and `awk … exit` are fine on a file argument or a
here-string; the rule is only about a **process** on the left of the `|`.
Scripts that run inside a pod or a kind node (`kubectl exec … sh -c '…'`,
`docker exec … sh -c '…'`) run without `pipefail` and are not affected.

### Evidence never fails on "no match" (`grep` exits 1 under `set -e`)

`grep` exits **1** when nothing matches, and under `pipefail` that is the
pipeline's status even when a `sed` or `cut` follows it. A forensics, summary
or "print the evidence" pipeline without a guard therefore aborts the script
exactly when the evidence it looks for is absent (#1143). Such a pipeline ends
in `|| true`, or filters with `awk` (which exits 0 on no match):

```bash
sed -n '1,200p' "$f" | grep -aE 'sendmsg|recvmsg' | sed -n '1,40p' || true
```

Gates are the opposite: a pipeline whose failure *should* stop the run belongs
in an `if` or ends in `|| die "…"`, so it fails with a message, not silently.

### A captured gate value never aborts before its check (`x="$(…)"` under `set -e`)

An assignment from a command substitution takes the substitution's exit
status, and under `set -e` a non-zero one aborts the script **on the
assignment** — so when a gate captures a value and checks it on the next line,
a producer that exits non-zero exactly when the thing is absent (`grep` with no
match, `stat`/`cat` of a missing path, `curl` that cannot connect, a
`kubectl exec` wrapping any of them) stops the run before the `die` that names
the cause can print (#1150). The gate still fails, but with no message, at the
wrong line. End the substitution in `|| true` and let the check carry the gate:

```bash
# Don't: no `/s` mount → grep exits 1 → silent exit here, the die never runs.
mnt="$(in_pod "$pod" "grep ' /s ' /proc/mounts")"
# Do:
mnt="$(in_pod "$pod" "grep ' /s ' /proc/mounts" || true)"
case "$mnt" in tmpfs\ /s\ tmpfs\ *) ;; *) die "/s in $pod is not a tmpfs mount: '$mnt'" ;; esac
```

The same holds for a helper function captured as `x="$(helper)"`: guard the
call (`x="$(helper || true)"`) or end the helper's pipeline in `|| true`. The
check after the capture must then reject the empty string, which an equality or
`case` test against the expected value, or `[ -n "$x" ] || die …`, already does.
