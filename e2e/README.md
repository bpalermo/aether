# e2e scripts

Kind harnesses for the mesh. Each script documents its own legs, knobs and
gates in its header; `pressure/` and `spike/` have their own READMEs. The
long-running soak is run by an external soak harness, maintained outside this repository.

Every harness that creates a kind cluster sources
[`kind-version.sh`](kind-version.sh) and passes `--image "$KIND_NODE_IMAGE"`, so
a local run and CI run the same Kubernetes; bumping it is one file
([runbook](../docs/runbook.md#bumping-the-e2e-kubernetes-version)).
`//e2e:kind_pin_test` enforces both.

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
grep -q "inbound_${pod}" <<<"$(admin /listeners 2>/dev/null)"
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
