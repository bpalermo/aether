#!/usr/bin/env bash
# Assert the shipped custom Envoy still carries a symbol table (aether #651/#653).
#
# Usage:
#   check_symtab.sh <binary>
#
# Why this is a release gate and not a nicety:
#
# The OpenTelemetry eBPF profiler never symbolizes native frames in-agent — it
# reports (build-ID, file offset) and leaves naming to the backend. Pyroscope's
# symbolizer then needs a symbol table for *this* build to turn those offsets
# into names. If `.symtab` ever disappears from the shipped binary, the ~1 core
# of fleet CPU the proxy burns silently goes back to `envoy!0x52e4c6f` hex
# frames — with no build error, no failing test, and nothing in the flamegraph
# to explain why. This test is the tripwire for that.
#
# The failure is easy to introduce by accident, because Bazel's strip semantics
# are not what the name suggests: `--strip=always` passes the linker
# `--strip-debug`, which removes DWARF but KEEPS `.symtab`. A future change to a
# real strip (`-Wl,-s` / `--strip-all`, or `objcopy --strip-all` in an image
# rule) would look like a harmless size optimisation and would cost the fleet
# its symbols. See the comment on `build:release --strip=always` in
# proxy/.bazelrc.
#
# Dependency-free on purpose: `od` + `awk` are POSIX, so this runs unchanged on a
# GitHub runner and inside a BuildBuddy RBE executor image, neither of which is
# guaranteed to ship binutils `readelf`/`nm`.
set -euo pipefail

binary=${1:?usage: check_symtab.sh <binary>}

if [ ! -f "$binary" ]; then
	echo "FAIL: $binary does not exist" >&2
	exit 1
fi

# le <hex-string-of-little-endian-bytes> -> decimal.
# od prints bytes in file order, so a little-endian field arrives reversed.
le() {
	local hex=$1 out="" i
	for ((i = ${#hex} - 2; i >= 0; i -= 2)); do out+=${hex:i:2}; done
	echo $((16#$out))
}

# read_at <offset> <count> -> contiguous lowercase hex of those bytes.
read_at() {
	od -An -v -tx1 -j "$1" -N "$2" "$binary" | tr -d ' \n'
}

# ELF64 header fields we need (all little-endian):
#   e_shoff     0x28, 8 bytes — section header table offset
#   e_shentsize 0x3a, 2 bytes — bytes per section header
#   e_shnum     0x3c, 2 bytes — number of section headers
header=$(read_at 0 64)
if [ "${header:0:8}" != "7f454c46" ]; then
	echo "FAIL: $binary is not an ELF file" >&2
	exit 1
fi
if [ "${header:8:2}" != "02" ]; then
	echo "FAIL: $binary is not ELF64 (ei_class=${header:8:2})" >&2
	exit 1
fi

shoff=$(le "${header:80:16}")
shentsize=$(le "${header:116:4}")
shnum=$(le "${header:120:4}")

if [ "$shoff" -eq 0 ] || [ "$shnum" -eq 0 ]; then
	echo "FAIL: $binary has no section header table — it has been fully stripped" >&2
	echo "      (aether #651: the shipped Envoy must retain .symtab so Pyroscope can" >&2
	echo "       symbolize native frames; see proxy/.bazelrc)" >&2
	exit 1
fi

# Walk the section headers looking for SHT_SYMTAB (sh_type == 2), which is what
# `.symtab` is by definition — matching on the type rather than the section name
# avoids a second read of the section-name string table. Each ELF64 section
# header is: sh_name(4) sh_type(4) sh_flags(8) sh_addr(8) sh_offset(8)
# sh_size(8) sh_link(4) sh_info(4) sh_addralign(8) sh_entsize(8).
table=$(read_at "$shoff" $((shnum * shentsize)))

symtab_size=0
symtab_entsize=0
for ((i = 0; i < shnum; i++)); do
	entry=${table:$((i * shentsize * 2)):$((shentsize * 2))}
	[ "$(le "${entry:8:8}")" -eq 2 ] || continue
	symtab_size=$(le "${entry:64:16}")
	symtab_entsize=$(le "${entry:112:16}")
	break
done

if [ "$symtab_size" -eq 0 ]; then
	echo "FAIL: $binary carries no .symtab (SHT_SYMTAB) section" >&2
	echo "      Native frames in Pyroscope will render as hex addresses." >&2
	echo "      (aether #651. Bazel's --strip=always keeps .symtab; something in the" >&2
	echo "       build has moved to a full strip — see proxy/.bazelrc)" >&2
	exit 1
fi

# An ELF64 symbol is 24 bytes; guard against a degenerate table that exists but
# holds nothing useful.
symbols=0
if [ "$symtab_entsize" -gt 0 ]; then
	symbols=$((symtab_size / symtab_entsize))
fi
if [ "$symbols" -lt 1000 ]; then
	echo "FAIL: $binary has only $symbols symbols in .symtab — expected a full Envoy table" >&2
	exit 1
fi

echo "OK: $binary retains .symtab ($symbols symbols, $symtab_size bytes)"
