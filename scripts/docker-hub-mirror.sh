#!/usr/bin/env bash
# Pull Docker Hub images on a CI runner without depending on Docker Hub's
# anonymous limit or on its token endpoint (#1602).
#
#   scripts/docker-hub-mirror.sh [<image>@sha256:<digest> ...]
#
# What failed: the runner's Docker pulled `kindest/node` from Docker Hub with no
# credential, and `auth.docker.io/token` timing out (or the anonymous rate
# limit) failed required jobs in which no test had failed. This repository
# holds no Docker Hub credential and this script needs none.
#
# What it does:
#   1. Adds a registry mirror (default https://mirror.gcr.io, Google's public
#      pull-through cache of Docker Hub) to the Docker daemon's configuration
#      and reloads the daemon (SIGHUP: `registry-mirrors` is one of the options
#      dockerd re-reads without a restart, so nothing running is disturbed and
#      no loaded image is lost). The daemon then asks the mirror first for
#      every Docker Hub pull and goes to Docker Hub only for what the mirror
#      does not answer.
#   2. For each image named: asks the mirror, anonymously, whether it serves
#      that DIGEST, then pulls the image by its unchanged reference (retried).
#      The reference keeps its digest, so what runs is the pinned content
#      whichever endpoint served it: the daemon verifies it.
#
# A reference without a digest is refused (exit 2): this script never pulls
# what is not pinned. Everything that would send a pull back to Docker Hub is a
# `::warning::` annotation on the run, never silence: the daemon could not be
# given the mirror, or the mirror does not serve the digest. Those do not fail
# the job, because Docker Hub is then still a working second source; a pull
# that fails on every attempt does (exit 1).
#
# An image on another registry (gcr.io, quay.io, registry.k8s.io) is pulled as
# it is: the mirror only stands in for Docker Hub.
#
# Environment (the defaults are what a GitHub-hosted runner needs):
#   DOCKER_HUB_MIRROR         the mirror's URL (default https://mirror.gcr.io)
#   DOCKER_DAEMON_JSON        the daemon's configuration file (default /etc/docker/daemon.json)
#   SUDO                      how to become root for the file and the reload (default sudo; empty for none)
#   HUB_MIRROR_PULL_ATTEMPTS  pull attempts per image (default 3)
#   HUB_MIRROR_RETRY_SLEEP    seconds before the 2nd attempt, doubled each time (default 10)
#   HUB_MIRROR_RELOAD_WAIT    seconds to wait for the reload to show in `docker info` (default 10)
#   JQ, CURL                  the jq and curl to run
#
# //scripts:docker_hub_mirror_test runs this against a fake docker, curl and
# systemctl; .github/actions/setup-kind is where CI calls it.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables
set -euo pipefail

MIRROR="${DOCKER_HUB_MIRROR:-https://mirror.gcr.io}"
MIRROR="${MIRROR%/}"
DAEMON_JSON="${DOCKER_DAEMON_JSON:-/etc/docker/daemon.json}"
SUDO="${SUDO-sudo}"
ATTEMPTS="${HUB_MIRROR_PULL_ATTEMPTS:-3}"
RETRY_SLEEP="${HUB_MIRROR_RETRY_SLEEP:-10}"
RELOAD_WAIT="${HUB_MIRROR_RELOAD_WAIT:-10}"
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"

log() { echo "docker-hub-mirror: $*"; }
warn() { echo "::warning::docker-hub-mirror: $*"; }

as_root() {
	if [ -n "$SUDO" ]; then
		"$SUDO" "$@"
	else
		"$@"
	fi
}

# mirror_listed: the RUNNING daemon uses the mirror (not: the file names it).
mirror_listed() {
	local mirrors
	mirrors="$(docker info --format '{{json .RegistryConfig.Mirrors}}' 2>/dev/null)" || return 1
	"$JQ" -e --arg m "$MIRROR" '(. // []) | map(sub("/+$"; "")) | index($m) != null' >/dev/null 2>&1 <<<"$mirrors"
}

# configure_daemon: 0 when the running daemon lists the mirror afterwards.
configure_daemon() {
	if mirror_listed; then
		log "the Docker daemon already uses $MIRROR"
		return 0
	fi
	local current new
	current='{}'
	if [ -s "$DAEMON_JSON" ]; then
		current="$(as_root cat "$DAEMON_JSON")" || {
			warn "cannot read $DAEMON_JSON; the daemon keeps pulling from Docker Hub only"
			return 1
		}
	fi
	# The mirror goes first; every other key of the file is kept as it is.
	new="$("$JQ" --arg m "$MIRROR" '.["registry-mirrors"] = ([$m] + ((.["registry-mirrors"] // []) - [$m]))' <<<"$current")" || {
		warn "$DAEMON_JSON is not a JSON object; left untouched, the daemon keeps pulling from Docker Hub only"
		return 1
	}
	if ! as_root mkdir -p "$(dirname -- "$DAEMON_JSON")" ||
		! printf '%s\n' "$new" | as_root tee "$DAEMON_JSON" >/dev/null; then
		warn "cannot write $DAEMON_JSON; the daemon keeps pulling from Docker Hub only"
		return 1
	fi
	as_root systemctl reload docker || {
		warn "'systemctl reload docker' failed; the daemon keeps pulling from Docker Hub only"
		return 1
	}
	# A reload is a signal: the daemon applies it a moment later.
	local waited=0
	until mirror_listed; do
		if [ "$waited" -ge "$RELOAD_WAIT" ]; then
			warn "the Docker daemon does not list $MIRROR ${RELOAD_WAIT}s after the reload; it keeps pulling from Docker Hub only"
			return 1
		fi
		sleep 1
		waited=$((waited + 1))
	done
	log "the Docker daemon now asks $MIRROR before Docker Hub"
}

# hub_repository <image>: the Docker Hub repository path of a reference
# (`library/` added to an official image), or nothing when the reference names
# another registry.
hub_repository() {
	local name="${1%%@*}" first
	# A tag is what follows the last ':' when no '/' follows it (a ':' before
	# a '/' is a registry port).
	case "${name##*/}" in *:*) name="${name%:*}" ;; esac
	first="${name%%/*}"
	if [ "$first" != "$name" ]; then
		case "$first" in
		docker.io | index.docker.io | registry-1.docker.io) name="${name#*/}" ;;
		localhost | *.* | *:*) return 0 ;;
		esac
	fi
	case "$name" in */*) ;; *) name="library/$name" ;; esac
	printf '%s' "$name"
}

# mirror_serves <repository> <digest>: the mirror answers a request for that
# manifest, without a credential, with that digest.
mirror_serves() {
	local headers status served
	headers="$("$CURL" -sS -I --max-time 20 --retry 2 \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"$MIRROR/v2/$1/manifests/$2" 2>&1)" || {
		PROBE="no answer"
		return 1
	}
	headers="${headers//$'\r'/}"
	status="$(awk 'toupper($1) ~ /^HTTP\// { s = $2 } END { print s }' <<<"$headers")"
	served="$(awk 'tolower($1) == "docker-content-digest:" { d = $2 } END { print d }' <<<"$headers")"
	PROBE="HTTP ${status:-?}"
	[ "$status" = 200 ] || return 1
	[ "$served" = "$2" ] || {
		PROBE="HTTP 200 with digest '${served}'"
		return 1
	}
}

pull() {
	local image="$1" n=1 wait="$RETRY_SLEEP"
	while :; do
		if docker pull "$image"; then
			return 0
		fi
		if [ "$n" -ge "$ATTEMPTS" ]; then
			echo "::error::docker-hub-mirror: could not pull $image in $ATTEMPTS attempts. No test ran: this is the registry or the network, so re-run the job (docs/runbook.md, \"CI: Docker Hub pulls\")."
			return 1
		fi
		warn "pull of $image failed (attempt $n/$ATTEMPTS); again in ${wait}s"
		sleep "$wait"
		wait=$((wait * 2))
		n=$((n + 1))
	done
}

digest_re='@sha256:[0-9a-f]{64}$'
for image in "$@"; do
	[[ "$image" =~ $digest_re ]] || {
		echo "::error::docker-hub-mirror: '$image' is not pinned by digest (<name>[:<tag>]@sha256:<64 hex>); refusing to pull it"
		exit 2
	}
done

configured=0
configure_daemon && configured=1

PROBE=""
for image in "$@"; do
	repository="$(hub_repository "$image")"
	digest="${image##*@}"
	if [ -z "$repository" ]; then
		log "$image is not a Docker Hub image; pulling it from its own registry"
	elif ! mirror_serves "$repository" "$digest"; then
		warn "$MIRROR does not serve $repository@$digest ($PROBE): the pull of $image falls back to Docker Hub's anonymous limit (#1602)"
	elif [ "$configured" -ne 1 ]; then
		warn "$MIRROR serves $repository@$digest, but the daemon was not given the mirror: the pull of $image goes to Docker Hub's anonymous limit (#1602)"
	else
		log "$MIRROR serves $repository@$digest; the daemon asks it first"
	fi
	pull "$image"
	# Evidence only: which names the local store holds that digest under.
	log "$image present as $(docker image inspect --format '{{json .RepoDigests}}' "$image" 2>&1 || true)"
done
