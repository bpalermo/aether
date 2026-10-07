// Package snapshotversion is the registrar's endpoint-snapshot version format:
// the resume token an agent presents on reconnect. The registrar server renders
// and classifies it (registrar/internal/server); the node agent compares it to
// decide whether an initial exchange was a resend (registry/internal/registrar).
// Both sides import this one parser so they cannot drift apart (#1272).
//
// Stdlib-only by design: the agent links it, and so must every binary that
// links the agent's registry client.
package snapshotversion

import (
	"strconv"
	"strings"
)

// Version format (issues #1193, #1203). The version is the resume token an
// agent presents on reconnect; the server skips the snapshot when the token
// names the contents it holds now. It is therefore a NAME OF THE CONTENTS, not
// a count of writes, and it is comparable across replicas:
//
//   - "<rev>.<hash>": the contents are exactly the external registry's
//     listing at store revision rev (a backend implementing
//     registry.RevisionedLister: etcd).
//   - "<rev>+<hash>": the contents started from the listing at rev but deviate
//     from it -- a write-behind intent was overlaid, or an agent RPC was
//     applied since.
//   - "hash:<hash>": the backend has no store revision (kubernetes, where a
//     listing is not a function of the list's resourceVersion: health depends
//     on the clock and locality on a separate node list).
//
// <hash> is HashLen hex digits of a sha256 over the canonical contents (sorted
// keys + deterministic proto encoding of each endpoint), and it ALONE decides
// whether a client is current: a token is current iff its hash part equals the
// current content hash. The revision is carried for the lag metrics and for
// operators, never trusted for equality -- a rebuilt etcd restarts its
// revisions at 1, and a binary that decodes a stored value differently derives
// different contents at the same revision. No form parses as a bare integer, so
// a pre-#1193 counter token never matches.
const (
	// HashPrefix introduces a version with no store revision.
	HashPrefix = "hash:"
	// CleanSep separates the revision from the hash of a clean listing.
	CleanSep = "."
	// DirtySep separates the revision from the hash of a deviated listing.
	DirtySep = "+"
	// HashLen is the number of hex digits of the content hash.
	HashLen = 16
	// ValueHexLen is the number of leading hex digits of the content hash that
	// ContentHashValue keeps: 13 digits = ValueBits bits.
	ValueHexLen = 13
	// ValueBits is the width of a ContentHashValue.
	ValueBits = 4 * ValueHexLen
)

// Format renders the version for the given state; see the format above. A
// revision <= 0 means the backend has none.
func Format(revision int64, dirty bool, contentHash string) string {
	if revision <= 0 {
		return HashPrefix + contentHash
	}
	rev := strconv.FormatInt(revision, 10)
	if dirty {
		return rev + DirtySep + contentHash
	}
	return rev + CleanSep + contentHash
}

// ContentHash returns the content hash a version embeds: the part after
// "hash:", or after the "." / "+" that follows the revision. A pre-#1193
// counter version, an empty version and a version with an empty hash part
// embed none.
func ContentHash(version string) (string, bool) {
	if h, ok := strings.CutPrefix(version, HashPrefix); ok {
		return h, h != ""
	}
	if i := strings.IndexAny(version, CleanSep+DirtySep); i >= 0 {
		h := version[i+1:]
		return h, h != ""
	}
	return "", false
}

// ContentHashValue is the content hash as a number a float64 holds exactly: its
// first ValueHexLen hex digits (the top 52 bits of the 64 the hash carries),
// as a non-negative integer below 2^52. It is what the registrar exports as the
// VALUE of aether.registrar.snapshot.content_hash (#1329), so that a metrics
// backend that stores float64 samples (Prometheus) can compare two replicas'
// hashes with no label that would outlive a superseded hash.
//
// Bit budget. A float64 has a 53-bit significand, so every integer in
// [0, 2^53] is exact and 53 bits would fit. 52 are kept because that is a
// whole number of hex digits: the value printed as %013x IS the first 13
// digits of the hash in the version string, which an operator can match by
// eye. The 12 bits dropped cost nothing that matters here: the value is a
// divergence detector between a handful of replicas at one revision, not a
// global identifier. Two replicas serving DIFFERENT contents report the same
// value with probability 2^-52 (about 2.2e-16) per compared pair; the resume
// token keeps using all HashLen digits.
//
// It takes a content hash (State.ContentHash, or the result of ContentHash),
// not a version. ok is false when the argument is shorter than ValueHexLen or
// is not hex; a caller exporting a gauge then reports nothing rather than a
// made-up value.
func ContentHashValue(contentHash string) (value int64, ok bool) {
	if len(contentHash) < ValueHexLen {
		return 0, false
	}
	// ParseUint, not ParseInt: a sign is not a hex digit. 52 bits always fit
	// an int64.
	v, err := strconv.ParseUint(contentHash[:ValueHexLen], 16, ValueBits)
	if err != nil {
		return 0, false
	}
	return int64(v), true
}

// Relation is how a presented token relates to a current version.
type Relation int

const (
	// Different: the token names other contents, or one of the two embeds no
	// content hash and they are not identical.
	Different Relation = iota
	// Same: the token is the current version, byte for byte.
	Same
	// Renamed: the token names the current contents under another name (same
	// content hash, another revision or another form).
	Renamed
)

// String names the relation for test and log output.
func (r Relation) String() string {
	switch r {
	case Same:
		return "same"
	case Renamed:
		return "renamed"
	default:
		return "different"
	}
}

// Compare classifies token against current. Only identity or the embedded
// content hash decides (see the version format); the revision never does. It
// gives no special meaning to an empty token: a caller that treats "no token"
// as "always resend" checks that first.
func Compare(token, current string) Relation {
	if token == current {
		return Same
	}
	th, ok := ContentHash(token)
	ch, currentOK := ContentHash(current)
	if ok && currentOK && th == ch {
		return Renamed
	}
	return Different
}
