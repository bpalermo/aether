package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"google.golang.org/protobuf/proto"
)

// Issue #1105: the per-resource version map, built OUTSIDE the snapshot
// cache's mutex and memoized across builds.
//
// Delta xDS needs a version per resource, and go-control-plane computes it
// lazily: the first delta watch to look at a snapshot calls
// Snapshot.ConstructVersionMap, which deterministically marshals and sha256s
// EVERY resource -- from inside SetSnapshot (respondDeltaWatches) or
// CreateDeltaWatch, i.e. while holding the cache mutex the ADS stream
// goroutine needs for every request it handles. On the 200m reference-cluster agent that
// was ~93% of a build and 200-870 ms of wall clock per snapshot, during which
// ODCDS answers, EDS subscriptions and drains all queued (#1086, #1103).
//
// So the snapshot is handed to SetSnapshot with its VersionMap already filled
// (ConstructVersionMap is a no-op on a non-nil map), and an unchanged resource
// is not re-marshalled at all: the map is the same sha256-of-deterministic-
// marshal go-control-plane would compute, taken from the previous build when
// the SAME proto object is published under the same name again.
//
// That reuse is sound iff a proto, once handed to a snapshot, is never mutated
// in place. The cache already has to hold that line for a different reason --
// go-control-plane's server goroutines marshal published resources without
// any agent lock, so an in-place mutation would be a torn-marshal data race
// (see the comment in cluster.go on rebuilding load assignments, and the
// proto.Clone in refreshEntryMTLSLocked). It is enforced, not assumed:
//
//   - every build that is an audit re-hashes every resource, memo hits
//     included, and a hit whose bytes no longer match its memoized version is
//     counted (aether.agent.snapshot.version_memo_mismatch), logged, and
//     published with the CORRECT version, so a violation costs at most one
//     audit interval of staleness, never a permanently stuck resource;
//   - in this package's tests every build is an audit and a mismatch panics
//     (versionmemo_strict_test.go), so the whole suite is the audit of every
//     builder.
type versionMemo struct {
	// entries is the previous build's resources and their versions, by type
	// URL then resource name. Replaced wholesale by every fill, so it never
	// holds a resource the current snapshot does not (no leak, and a
	// memoized pointer can never be freed and reused while it is a key).
	entries map[string]map[string]memoEntry
	// auditEvery is how often a build re-hashes everything. <= 0 audits
	// every build.
	auditEvery time.Duration
	lastAudit  time.Time
	// strict panics on a mismatch (tests only).
	strict bool
	// buf is the marshal buffer reused across a fill's resources.
	buf []byte
}

type memoEntry struct {
	res     types.Resource
	version string
}

// defaultVersionMemoAuditEvery bounds the staleness a memo violation could
// cause. A build that is an audit costs what every build cost before #1105,
// but still outside the cache mutex.
const defaultVersionMemoAuditEvery = time.Minute

// maxMismatchNames bounds the resource names a mismatch log line carries.
const maxMismatchNames = 10

// memoStats is what one fill did.
type memoStats struct {
	hits, hashed int
	audit        bool
	mismatches   []string // "<type>/<name>", capped at maxMismatchNames
	mismatchN    int
}

// versionMemoAuditEvery and versionMemoStrict seed every new memo. They are
// variables only so this package's tests can make every build a strict audit
// (versionmemo_strict_test.go); production never writes them.
var (
	versionMemoAuditEvery = defaultVersionMemoAuditEvery
	versionMemoStrict     = false
)

// newVersionMemo returns an empty memo.
func newVersionMemo() *versionMemo {
	return &versionMemo{auditEvery: versionMemoAuditEvery, strict: versionMemoStrict}
}

// fill computes s.VersionMap -- exactly the map s.ConstructVersionMap would
// build -- reusing the previous build's version for every resource that is the
// same proto object under the same name, and remembers this build for the
// next. Caller must serialize fills (generateSnapshot holds snapshotMu) and
// must call it before s is handed to SetSnapshot: the map is written here and
// only read afterwards.
func (m *versionMemo) fill(s *cachev3.Snapshot, now time.Time) (memoStats, error) {
	if s == nil {
		return memoStats{}, errors.New("missing snapshot")
	}
	st := memoStats{audit: m.auditEvery <= 0 || m.lastAudit.IsZero() || now.Sub(m.lastAudit) >= m.auditEvery}
	versions := make(map[string]map[string]string, len(s.Resources))
	next := make(map[string]map[string]memoEntry, len(s.Resources))
	for i, group := range s.Resources {
		typeURL, err := cachev3.GetResponseTypeURL(types.ResponseType(i))
		if err != nil {
			return st, err
		}
		prev := m.entries[typeURL]
		vs := make(map[string]string, len(group.Items))
		kept := make(map[string]memoEntry, len(group.Items))
		for _, item := range group.Items {
			name := cachev3.GetResourceName(item.Resource)
			v, err := m.version(&st, typeURL, name, item.Resource, prev)
			if err != nil {
				return st, err
			}
			vs[name] = v
			kept[name] = memoEntry{res: item.Resource, version: v}
		}
		versions[typeURL] = vs
		next[typeURL] = kept
	}
	s.VersionMap = versions
	m.entries = next
	if st.audit {
		m.lastAudit = now
	}
	return st, nil
}

// version returns one resource's version: the memoized one for a hit outside
// an audit, a fresh hash otherwise (checked against the memo on an audit).
func (m *versionMemo) version(st *memoStats, typeURL, name string, r types.Resource, prev map[string]memoEntry) (string, error) {
	e, ok := prev[name]
	hit := ok && e.res == r
	if hit && !st.audit {
		st.hits++
		return e.version, nil
	}
	v, err := m.hash(r)
	if err != nil {
		return "", fmt.Errorf("version %s %q: %w", typeURL, name, err)
	}
	st.hashed++
	if hit && v != e.version {
		// The same proto object now marshals to different bytes: someone
		// mutated a published resource in place.
		if m.strict {
			panic(fmt.Sprintf("version memo: %s %q was mutated in place after it was published (#1105)", typeURL, name))
		}
		st.mismatchN++
		if len(st.mismatches) < maxMismatchNames {
			st.mismatches = append(st.mismatches, typeURL+"/"+name)
		}
	}
	return v, nil
}

// deterministic is cachev3.MarshalResource's encoding.
var deterministic = proto.MarshalOptions{Deterministic: true}

// hash is go-control-plane's own per-resource version: hex(sha256) of the
// deterministic marshal (cachev3.MarshalResource + cachev3.HashResource, as
// Snapshot.ConstructVersionMap computes it), so a version computed here is
// byte-for-byte the one go-control-plane would have computed -- and the one
// an Envoy reconnecting with initial_resource_versions carries. The marshal
// buffer is reused across resources (fills are serialized), which keeps a
// build's hashing from allocating a fresh buffer per resource.
func (m *versionMemo) hash(r types.Resource) (string, error) {
	b, err := deterministic.MarshalAppend(m.buf[:0], r)
	if err != nil {
		return "", err
	}
	m.buf = b
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
