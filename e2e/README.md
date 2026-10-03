# e2e scripts

Kind (and talos-main soak) harnesses for the mesh. Each script documents its
own legs, knobs and gates in its header; `soak/`, `pressure/` and `spike/` have
their own READMEs.

## Rules for script authors

### No early-exit reader in a pipeline (SIGPIPE under `pipefail`)

Every script here runs under `set -euo pipefail` (the soak samplers under
`set -uo pipefail`). A pipeline whose **reader exits before its writer is
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
