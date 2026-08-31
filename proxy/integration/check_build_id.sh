#!/usr/bin/env bash
# Assert the GNU build-ID baked into a linked ELF binary (aether #651).
#
# Usage:
#   check_build_id.sh <binary> [<expected>]
#
# <expected> may be
#   * a 40-hex SHA — the build-ID must equal it exactly;
#   * a path to Envoy's `gnu_build_id.ldscript` (`--build-id=0x<sha>`) — the
#     build-ID must equal the id the linker was handed, which proves the stamped
#     link actually took effect;
#   * omitted or "-" — presence check only.
#
# Deliberately dependency-free: `od` + `awk` are POSIX, so this runs unchanged on
# a GitHub runner and inside a BuildBuddy RBE executor image, neither of which is
# guaranteed to ship binutils `readelf`.
set -euo pipefail

binary=${1:?usage: check_build_id.sh <binary> [<expected>]}
expected_arg=${2:--}

# The ELF note header for a 20-byte NT_GNU_BUILD_ID, little-endian:
#   namesz=4 | descsz=0x14 | type=3 | "GNU\0"
# Both architectures aether ships (x86-64, aarch64) are little-endian.
note_prefix="040000001400000003000000474e5500"

# extract_build_id <file> [byte-limit]
# Streams the file through od/awk with a sliding window, so a match that straddles
# an od line — or a 150 MB Envoy binary — is handled without buffering the lot.
extract_build_id() {
	local file=$1 limit=${2:-}
	local od_args=(-An -v -tx1)
	[ -n "$limit" ] && od_args+=(-N "$limit")
	# awk exits as soon as it has the id, which SIGPIPEs od; that is the point of
	# the early exit, so pipefail is off for this pipeline only.
	set +o pipefail
	od "${od_args[@]}" "$file" 2>/dev/null | awk -v pfx="$note_prefix" '
    BEGIN { plen = length(pfx) }
    {
      gsub(/ /, "", $0)
      buf = buf $0
      i = index(buf, pfx)
      if (i > 0 && length(buf) >= i + plen + 39) {
        print substr(buf, i + plen, 40)
        exit
      }
      if (length(buf) > 8192) buf = substr(buf, length(buf) - 200)
    }
  '
	set -o pipefail
}

if [ ! -f "$binary" ]; then
	echo "FAIL: $binary does not exist" >&2
	exit 1
fi

# The note lives in the first PT_LOAD, a few hundred bytes in; 4 MiB is a wildly
# generous fast path. Fall back to the whole file rather than reporting a false
# "no build-ID" if a future linker moves it.
got=$(extract_build_id "$binary" 4194304)
if [ -z "$got" ]; then
	got=$(extract_build_id "$binary")
fi

if [ -z "$got" ]; then
	echo "FAIL: $binary carries no .note.gnu.build-id" >&2
	echo "      (aether #651: released binaries must be linked with a stamped build-ID)" >&2
	exit 1
fi

if [ "$expected_arg" = "-" ]; then
	echo "OK: $binary build-ID $got (presence only)"
	exit 0
fi

if [ -f "$expected_arg" ]; then
	# Envoy's ldscript: the exact linker argument the stamped link consumed.
	expected=$(tr -d ' \r\n' <"$expected_arg" |
		sed -n 's/.*--build-id=0x\([0-9a-fA-F]\{40\}\).*/\1/p')
	if [ -z "$expected" ]; then
		echo "FAIL: $expected_arg holds no 40-hex --build-id=0x<sha>" >&2
		echo "      contents: $(cat "$expected_arg")" >&2
		exit 1
	fi
else
	expected=$expected_arg
fi
expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')

if [ "$got" != "$expected" ]; then
	echo "FAIL: $binary build-ID is $got, want $expected" >&2
	echo "      (aether #651: the build-ID must be the release commit SHA)" >&2
	exit 1
fi

echo "OK: $binary build-ID $got"
