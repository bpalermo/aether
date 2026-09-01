#!/usr/bin/env bash
# Assert the GNU build-ID of the linked custom Envoy (aether #651, #653).
#
# Usage:
#   check_build_id.sh <binary>
#
# What it proves, and why that is the right assertion here:
#
#   * the binary carries a `.note.gnu.build-id` at all — without one the eBPF
#     profiler has no key to join samples to uploaded symbols (#651);
#   * the descriptor is exactly 20 bytes, i.e. SHA-1 shaped. That width is what
#     pins the *kind* of ID: the hermetic toolchain's own `--build-id=md5` gives
#     16 bytes and `--build-id=fast` gives 8, so 20 bytes means the
#     `--build-id=sha1` --linkopt in proxy/.bazelrc was the last one on the link
#     command line and lld's content hash of the linked output is what landed
#     (#653);
#   * the descriptor is not all zeroes (a hex-string build-ID placeholder).
#
# It deliberately does NOT recompute the hash: lld's `--build-id=sha1` is a tree
# hash over the output buffer, an internal detail of the linker rather than a
# published function of the file, so recomputing it in a shell test would assert
# an lld implementation detail. That the flag is what produced the note is
# asserted at the source instead — proxy-release.yml greps the CppLink action's
# command line for `--build-id=sha1` before publishing.
#
# Dependency-free on purpose: `od` + `awk` are POSIX, so this runs unchanged on a
# GitHub runner and inside a BuildBuddy RBE executor image, neither of which is
# guaranteed to ship binutils `readelf`.
set -euo pipefail

binary=${1:?usage: check_build_id.sh <binary>}

# An ELF note is: namesz | descsz | type | name | desc, each header field a
# 32-bit little-endian word (both architectures aether ships are little-endian).
# Anchor on the fixed tail of the header — type=NT_GNU_BUILD_ID(3) followed by
# the NUL-terminated owner "GNU" — then read namesz/descsz back off the front,
# so a note of any descriptor width is *found* and can be reported as wrong
# rather than looking like no note at all.
anchor="03000000474e5500"

# extract_note <file> [byte-limit] -> "<descsz-hex> <first-20-desc-bytes-hex>"
# Streams the file through od/awk with a sliding window, so a match that straddles
# an od line — or a 150 MB Envoy binary — is handled without buffering the lot.
extract_note() {
	local file=$1 limit=${2:-}
	local od_args=(-An -v -tx1)
	[ -n "$limit" ] && od_args+=(-N "$limit")
	# awk exits as soon as it has the note, which SIGPIPEs od; that is the point
	# of the early exit, so pipefail is off for this pipeline only.
	set +o pipefail
	od "${od_args[@]}" "$file" 2>/dev/null | awk -v anchor="$anchor" '
    BEGIN { alen = length(anchor) }
    {
      gsub(/ /, "", $0)
      buf = buf $0
      i = index(buf, anchor)
      if (i > 16 && length(buf) >= i + alen + 39) {
        if (substr(buf, i - 16, 8) == "04000000") {   # namesz == 4 ("GNU\0")
          print substr(buf, i - 8, 8) " " substr(buf, i + alen, 40)
          exit
        }
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
note=$(extract_note "$binary" 4194304)
if [ -z "$note" ]; then
	note=$(extract_note "$binary")
fi

if [ -z "$note" ]; then
	echo "FAIL: $binary carries no .note.gnu.build-id" >&2
	echo "      (aether #651: released binaries must be linked with --build-id=sha1;" >&2
	echo "       see the --linkopt in proxy/.bazelrc)" >&2
	exit 1
fi

descsz=${note%% *}
got=${note#* }

# 0x00000014 little-endian == 20 bytes == SHA-1.
if [ "$descsz" != "14000000" ]; then
	width=$((16#${descsz:6:2}${descsz:4:2}${descsz:2:2}${descsz:0:2}))
	echo "FAIL: $binary has a $width-byte build-ID descriptor, want 20" >&2
	echo "      (aether #653: the id must be lld's --build-id=sha1 content hash, not the" >&2
	echo "       toolchain's 16-byte uuid and not an 8-byte --build-id=fast hash)" >&2
	exit 1
fi

if [ "$got" = "0000000000000000000000000000000000000000" ]; then
	echo "FAIL: $binary has an all-zero build-ID" >&2
	exit 1
fi

echo "OK: $binary build-ID $got (20-byte content hash)"
