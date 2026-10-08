#!/usr/bin/env bash
# Which of a publish's artifacts get a build-provenance attestation from THIS
# run, the check that every one of them has a verifiable one afterwards, and
# the run summary that says which digests are new (#1378). The steps around
# `actions/attest` in .github/workflows/publish.yaml.
#
#   scripts/publish-provenance.sh subjects <refs file>...
#   scripts/publish-provenance.sh verify <full sha>
#   scripts/publish-provenance.sh summary <full sha> <images state> <charts state>
#
# THE RULE: ATTEST ONLY WHAT HAS NO PROVENANCE YET. An image whose inputs did
# not change keeps its digest from one commit to the next. Attesting it on every
# publish would pile one statement per commit on the same digest (about
# seventeen a day; `gh attestation verify` reads 30 by default), so a digest is
# attested ONCE, by the first run that pushed it, and its provenance names the
# commit of that run. The same question is asked of every reference, image or
# chart, with no special case. A chart is new on every commit by construction
# (its `X.Y.Z-<sha>` version carries the commit, and the `appVersion` of every
# chart, aether's bare `X.Y.Z` one included, is the output of `git describe`),
# so every publish attests every chart; a re-run of a publish finds everything
# attested and creates nothing.
#
# `subjects` reads `<registry>/<repository>@sha256:<digest>` lines (the files
# the sign steps wrote, in the order given) and decides, per digest, from
# GitHub's attestation store and never from the commit. TWO questions:
#
#   1. Does the store list a provenance attestation for the digest?
#      GET /repos/<repository>/attestations/<digest>, filtered to the provenance
#      predicate. THREE answers, never two (measured against the real store:
#      a digest with one answers 200 and a list, one without answers 404):
#        200 with at least one     it lists one: question 2
#        404, or 200 with none     it has none: a subject of this run
#        anything else             unknown. Asked again, PROVENANCE_ATTEMPTS
#                                  times in all; still unknown FAILS THE STEP.
#                                  An API error is never "none" (that would
#                                  attest the digest again on every hiccup) and
#                                  never "it has one" (that would leave it
#                                  without provenance).
#      The HTTP status is the answer, not the wording of an error message.
#   2. Does what is listed VERIFY as this repository's publish workflow?
#      `gh attestation verify oci://<ref> --repo --signer-workflow
#      --predicate-type`, WITHOUT `--source-digest`: the attestation of an
#      unchanged digest names the commit that built it first, not this one.
#        yes   "already attested": SKIPPED, and the commit it names is printed
#        no    a subject of this run after all, with a ::warning:: (attested by
#              something else, or by this workflow under another name). A
#              listed attestation nobody can verify is not provenance, and a
#              second statement is harmless where a missing one is not.
#
# It writes, in the working directory:
#   provenance-subjects.sha256  `<hex digest>  <registry>/<repository>` per
#                               subject: the sha256sum format actions/attest
#                               reads. The name carries no tag; the digest is
#                               the identity. EMPTY when nothing is new: the
#                               workflow must not hand it to actions/attest.
#   provenance-new.txt          the references this run attests
#   provenance-existing.txt     the references skipped as attested already
#   provenance-state.tsv        `<ref> TAB <new|unchanged|reattested>`
# and `subjects=<n>` / `existing=<n>` to $GITHUB_OUTPUT. The workflow runs the
# attest step only when `subjects` is not 0, and this step says in one line how
# many were new and how many were skipped, so "attested nothing" is never
# silent.
#
# `verify` proves the store answers the way a user would ask it, for EVERY
# reference of the two lists:
#   - attested by this run: `gh attestation verify` with the repository, the
#     signer workflow, the predicate type AND `--source-digest <sha>`: built
#     from this commit;
#   - skipped: the same without `--source-digest`, and the commit, run and time
#     the existing attestation names are printed (the oldest first when there is
#     more than one: the order `gh` returns them in is not relied on). A skipped
#     reference with NO verifiable attestation fails the step: the skip and the
#     store disagree, and nothing here guesses which is right.
# Each check is retried PROVENANCE_ATTEMPTS times (the store can trail an upload
# by a moment); every reference is checked even after a failure. It writes
# provenance-origin.tsv, `<ref> TAB <commit> TAB <run>`, for the summary.
#
# `--limit`. `gh attestation verify` fetches 30 attestations of a digest unless
# told otherwise and verifies only those. With this rule a digest has one, so
# 30 is never reached -- but a digest that did collect more (a workflow that
# attested every commit, a repair) would fail `--source-digest <new commit>` as
# soon as the matching statement fell outside the 30 fetched, for a reason no
# code change caused. Every call here passes PROVENANCE_LIMIT (default 1000,
# the most `gh` accepts), so the check does not depend on the count.
#
# `summary` prints the run summary as Markdown: one row per artifact with its
# digest, whether the digest is new, what was signed, the commit its provenance
# names and the tag it was pushed under. It reads the two `<out file>.state`
# files scripts/publish-sign.sh wrote and the two .tsv files above. Reporting
# only: nothing is verified from it.
#
# Environment:
#   GITHUB_REPOSITORY     owner/name (required)
#   GH_TOKEN              for `gh` (reads only)
#   SIGNER_WORKFLOW       default <GITHUB_REPOSITORY>/.github/workflows/publish.yaml
#   PROVENANCE_ATTEMPTS   tries per question (default 4)
#   PROVENANCE_INTERVAL   seconds; the wait after attempt n is n times this
#                         (default 5: 5, 10, 15 s)
#   PROVENANCE_LIMIT      attestations fetched per digest (default 1000)
#
# READ-ONLY towards GitHub and the registry: this script never attests.
#
# Exit: 0 done; 1 a question stayed unanswered, a reference did not verify, or
# the lists do not add up; 2 usage.
set -euo pipefail

mode="${1:-}"
repo_slug="${GITHUB_REPOSITORY:-}"
predicate='https://slsa.dev/provenance/v1'
# The same, as a query value: `gh api` takes an argument holding `://` for a
# whole URL.
predicate_query='https%3A%2F%2Fslsa.dev%2Fprovenance%2Fv1'
attempts="${PROVENANCE_ATTEMPTS:-4}"
interval="${PROVENANCE_INTERVAL:-5}"
limit="${PROVENANCE_LIMIT:-1000}"

usage() {
	echo "usage: $(basename "$0") subjects <refs file>... | verify <full 40-char commit sha> | summary <full 40-char commit sha> <images state> <charts state>" >&2
	exit 2
}
[ "$mode" = subjects ] || [ "$mode" = verify ] || [ "$mode" = summary ] || usage
if [ -z "$repo_slug" ]; then
	echo "::error::GITHUB_REPOSITORY is not set" >&2
	exit 2
fi
if ! [[ "$attempts" =~ ^[1-9][0-9]*$ ]] || ! [[ "$interval" =~ ^[0-9]+$ ]] || ! [[ "$limit" =~ ^[1-9][0-9]*$ ]]; then
	echo "::error::PROVENANCE_ATTEMPTS and PROVENANCE_LIMIT must be whole numbers above 0 and PROVENANCE_INTERVAL a whole number" >&2
	exit 2
fi
signer="${SIGNER_WORKFLOW:-${repo_slug}/.github/workflows/publish.yaml}"

ref_re='^.+@sha256:[0-9a-f]{64}$'

# Does the store list a provenance attestation for <digest>?
#   0  yes   1  no   2  unknown after every attempt (the last answer on stderr)
attestation_exists() {
	local digest="$1" i=1 out status count why
	out="$(mktemp)"
	while :; do
		# -i: the status line first. `gh api` exits non-zero on a 404 too, so
		# its exit status alone cannot tell "none" from "no answer".
		gh api -i "repos/${repo_slug}/attestations/${digest}?per_page=1&predicate_type=${predicate_query}" \
			>"$out" 2>/dev/null || true
		status="$(head -1 "$out" | tr -d '\r' | sed -nE 's|^HTTP/[0-9.]+ ([0-9]{3}).*|\1|p')"
		case "$status" in
		404)
			rm -f "$out"
			return 1
			;;
		200)
			if count="$(sed '1,/^\r\{0,1\}$/d' "$out" | python3 -c '
import json, sys
doc = json.load(sys.stdin)
att = doc.get("attestations") if isinstance(doc, dict) else None
if not isinstance(att, list):
    sys.exit(1)
print(len(att))
' 2>/dev/null)"; then
				rm -f "$out"
				[ "$count" -gt 0 ] && return 0
				return 1
			fi
			why="answered 200 with a body that is not a list of attestations"
			;;
		"") why="gave no HTTP answer" ;;
		*) why="answered HTTP ${status}" ;;
		esac
		if [ "$i" -ge "$attempts" ]; then
			echo "  the attestation store ${why} for ${digest} (${i} attempt(s))" >&2
			rm -f "$out"
			return 2
		fi
		echo "  the attestation store ${why} for ${digest} (attempt ${i} of ${attempts}); asking again in $((i * interval))s" >&2
		sleep $((i * interval))
		i=$((i + 1))
	done
}

# `gh attestation verify` for one reference, retried. Extra arguments are
# passed through; the JSON of the last attempt is left in $verify_out.
verify_out=""
verify_ref() {
	local ref="$1" i=1
	shift
	while :; do
		if gh attestation verify "oci://${ref}" \
			--repo "$repo_slug" \
			--signer-workflow "$signer" \
			--predicate-type "$predicate" \
			--limit "$limit" \
			--format json "$@" >"$verify_out"; then
			return 0
		fi
		if [ "$i" -ge "$attempts" ]; then
			return 1
		fi
		echo "  attestation for ${ref} not verifiable yet (attempt ${i} of ${attempts})"
		sleep $((i * interval))
		i=$((i + 1))
	done
}

# What the verified attestations in `gh attestation verify --format json` (on
# stdin) say about where the digest came from, the OLDEST one:
#   <commit> TAB <run> TAB <time> TAB <how many verified>
# Fields as the real store returns them (gh 2.102): the commit is
# predicate.buildDefinition.resolvedDependencies[].digest.gitCommit, and the
# certificate's sourceRepositoryDigest says the same.
origin_of() {
	python3 -c '
import json, sys
from datetime import datetime

def when(s):
    try:
        return datetime.fromisoformat(s)
    except (TypeError, ValueError):
        return None

rows = []
for r in json.load(sys.stdin):
    vr = r.get("verificationResult") or {}
    pred = (vr.get("statement") or {}).get("predicate") or {}
    deps = (pred.get("buildDefinition") or {}).get("resolvedDependencies") or []
    commit = next((d.get("digest", {}).get("gitCommit") for d in deps if isinstance(d, dict) and d.get("digest", {}).get("gitCommit")), None)
    if not commit:
        commit = ((vr.get("signature") or {}).get("certificate") or {}).get("sourceRepositoryDigest")
    run = ((pred.get("runDetails") or {}).get("metadata") or {}).get("invocationId") or "run not recorded"
    stamps = vr.get("verifiedTimestamps") or [{}]
    ts = stamps[0].get("timestamp") if isinstance(stamps[0], dict) else None
    rows.append((when(ts), ts or "time not recorded", commit or "commit not readable", run))
if not rows:
    sys.exit(1)
rows.sort(key=lambda x: (x[0] is None, x[0].timestamp() if x[0] else 0))
_, ts, commit, run = rows[0]
print("\t".join((commit, run, ts, str(len(rows)))))
'
}

# origin_of for the last verify_ref, left in three variables: the commit, the
# run, and the words a log line carries. Call it plainly, not in $(...).
origin_commit="" origin_run="" origin_words=""
read_origin() {
	local ts="" n=""
	origin_commit="" origin_run=""
	IFS=$'\t' read -r origin_commit origin_run ts n < <(origin_of <"$verify_out" 2>/dev/null) || true
	if [ -z "$origin_commit" ]; then
		origin_commit="commit not readable" origin_run="run not recorded"
		origin_words="its origin could not be read from the statement"
	elif [ "${n:-1}" = 1 ]; then
		origin_words="built first by commit ${origin_commit} (${origin_run}, ${ts})"
	else
		origin_words="built first by commit ${origin_commit} (${origin_run}, ${ts}; ${n} attestations verify, this is the oldest)"
	fi
}

read_refs() {
	local f="$1" line
	[ -r "$f" ] || return 0
	while read -r line; do
		[ -n "$line" ] && printf '%s\n' "$line"
	done <"$f"
}

if [ "$mode" = subjects ]; then
	shift
	[ "$#" -ge 1 ] || usage
	refs=()
	for f in "$@"; do
		if [ ! -s "$f" ]; then
			echo "::error::${f} is missing or empty -- the sign step recorded nothing"
			exit 1
		fi
		while read -r line; do
			refs+=("$line")
		done < <(read_refs "$f")
	done

	verify_out="$(mktemp)"
	trap 'rm -f "$verify_out"' EXIT
	: >provenance-subjects.sha256
	: >provenance-new.txt
	: >provenance-existing.txt
	: >provenance-state.tsv
	for ref in "${refs[@]}"; do
		if ! [[ "$ref" =~ $ref_re ]]; then
			echo "::error::not a <registry>/<repository>@sha256:<digest> reference: ${ref}"
			exit 1
		fi
		digest="${ref##*@}"
		state=0
		attestation_exists "$digest" || state=$?
		what=new
		case "$state" in
		0)
			if verify_ref "$ref"; then
				read_origin
				echo "already attested: ${ref} (${origin_words})"
				echo "$ref" >>provenance-existing.txt
				printf '%s\tunchanged\n' "$ref" >>provenance-state.tsv
				continue
			fi
			echo "::warning::the attestation store lists provenance for ${ref}, but none of it verifies as ${signer}: attesting it now (a second statement is harmless, provenance nobody can verify is not provenance)"
			what=reattested
			;;
		1) ;;
		*)
			echo "::error::could not tell whether ${ref} has a provenance attestation (see above): not attesting on a guess, and not skipping on one. Re-run the workflow; what is signed and attested already is skipped."
			exit 1
			;;
		esac
		echo "new subject:      ${ref}"
		echo "$ref" >>provenance-new.txt
		printf '%s\t%s\n' "$ref" "$what" >>provenance-state.tsv
		printf '%s  %s\n' "${digest#sha256:}" "${ref%@*}" >>provenance-subjects.sha256
	done

	new="$(wc -l <provenance-new.txt)"
	existing="$(wc -l <provenance-existing.txt)"
	if [ $((new + existing)) -ne "${#refs[@]}" ] || [ "$(wc -l <provenance-subjects.sha256)" -ne "$new" ]; then
		echo "::error::${new} new + ${existing} already attested from ${#refs[@]} signed reference(s)"
		exit 1
	fi
	if [ -n "${GITHUB_OUTPUT:-}" ]; then
		printf 'subjects=%s\nexisting=%s\n' "$new" "$existing" >>"$GITHUB_OUTPUT"
	fi
	echo "${#refs[@]} signed reference(s): ${new} new provenance subject(s), ${existing} already attested (skipped)"
	if [ "$new" -eq 0 ]; then
		echo "::notice::provenance: nothing new to attest -- all ${existing} artifact(s) of this run already carry this workflow's provenance (a re-run, or a commit that changed no artifact)"
	else
		cat provenance-subjects.sha256
	fi
	exit 0
fi

commit="${2:-}"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || usage

if [ "$mode" = summary ]; then
	[ "$#" -eq 4 ] || usage
	for f in "$3" "$4" provenance-state.tsv provenance-origin.tsv; do
		if [ ! -s "$f" ]; then
			echo "::error::${f} is missing or empty -- no summary to write"
			exit 1
		fi
	done
	python3 - "$commit" "$3" "$4" <<'PY'
import sys

commit, images, charts = sys.argv[1:4]


def table(path, width):
    rows = {}
    with open(path) as f:
        for line in f:
            cols = line.rstrip("\n").split("\t")
            if len(cols) >= width:
                rows[cols[0]] = cols[1:]
    return rows


state = table("provenance-state.tsv", 2)
origin = table("provenance-origin.tsv", 3)
signed_words = {"signed-now": "signed by this run", "already-signed": "already signed"}

lines = []
counts = []
for kind, path in (("image", images), ("chart", charts)):
    new = total = 0
    with open(path) as f:
        for line in f:
            cols = line.rstrip("\n").split("\t")
            if len(cols) < 3:
                continue
            ref, tags, signed = cols[:3]
            name, digest = ref.rsplit("@", 1)
            what = state.get(ref, ["not recorded"])[0]
            total += 1
            if what == "new":
                new += 1
            elif what == "reattested":
                what = "unchanged (provenance attested again)"
            built = origin.get(ref, ["not recorded", ""])[0]
            lines.append(
                "| `%s` | `%s` | %s | %s | `%s` | %s |"
                % (
                    name.rsplit("/", 1)[-1],
                    digest,
                    "**new**" if what == "new" else what,
                    signed_words.get(signed, signed),
                    built,
                    ", ".join("`%s`" % t for t in tags.split(",")),
                )
            )
    counts.append("%d of %d %s digests are new" % (new, total, kind))

print("### Published artifacts of %s" % commit)
print()
print("**%s; %s.**" % tuple(counts))
print()
print(
    "A digest is new when no provenance existed for it before this run. An unchanged "
    "digest keeps the signature and the provenance of the commit that built it first, "
    "and a workload that pins it does not roll."
)
print()
print("| Artifact | Digest | Digest is | Signature | Provenance names commit | Tag pushed |")
print("| --- | --- | --- | --- | --- | --- |")
print("\n".join(lines))
PY
	exit 0
fi

if [ ! -e provenance-new.txt ] || [ ! -e provenance-existing.txt ]; then
	echo "::error::provenance-new.txt / provenance-existing.txt are missing -- the subjects step did not run"
	exit 1
fi

verify_out="$(mktemp)"
trap 'rm -f "$verify_out"' EXIT
: >provenance-origin.tsv
verified=0
failed=0
while read -r ref; do
	if verify_ref "$ref" --source-digest "$commit"; then
		echo "  verified provenance of ${ref} (attested by this run, commit ${commit})"
		read_origin
		printf '%s\t%s\t%s\n' "$ref" "$origin_commit" "$origin_run" >>provenance-origin.tsv
		verified=$((verified + 1))
	else
		echo "::error::no verifiable provenance attestation for ${ref} from commit ${commit}"
		failed=$((failed + 1))
	fi
done < <(read_refs provenance-new.txt)

while read -r ref; do
	if verify_ref "$ref"; then
		read_origin
		echo "  verified provenance of ${ref} (existing: ${origin_words})"
		printf '%s\t%s\t%s\n' "$ref" "$origin_commit" "$origin_run" >>provenance-origin.tsv
		verified=$((verified + 1))
	else
		echo "::error::${ref} was skipped as already attested, but no attestation of it verifies as ${signer}: the skip and the attestation store disagree"
		failed=$((failed + 1))
	fi
done < <(read_refs provenance-existing.txt)

if [ "$failed" -gt 0 ]; then
	echo "::error::${failed} artifact(s) without verifiable provenance, ${verified} with"
	exit 1
fi
if [ "$verified" -eq 0 ]; then
	echo "::error::verified no attestation"
	exit 1
fi
echo "verified the provenance of ${verified} artifact(s)"
