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
