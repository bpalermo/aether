package snapshotversion

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	h1 = "0123456789abcdef"
	h2 = "fedcba9876543210"
)

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int64
		dirty    bool
		want     string
	}{
		{"clean", 42, false, "42." + h1},
		{"dirty", 42, true, "42+" + h1},
		{"no revision", 0, false, "hash:" + h1},
		{"no revision ignores dirty", 0, true, "hash:" + h1},
		{"negative revision has none", -1, false, "hash:" + h1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Format(tc.revision, tc.dirty, h1))
		})
	}
}

func TestContentHash(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    string
		wantOK  bool
	}{
		{"clean", "42." + h1, h1, true},
		{"dirty", "42+" + h1, h1, true},
		{"fallback (kubernetes backend)", "hash:" + h1, h1, true},
		{"legacy bare counter", "17", "", false},
		{"empty", "", "", false},
		{"fallback with empty hash", "hash:", "", false},
		{"clean with empty hash", "42.", "", false},
		{"dirty with empty hash", "42+", "", false},
		{"garbage without separator", "not-a-version", "", false},
		// The parser takes everything after the FIRST separator: it does not
		// validate the revision or the hash, it only extracts. Pinned so a
		// stricter parser is a deliberate change on both sides at once.
		{"garbage before separator", "x.y", "y", true},
		{"first separator wins", "1.2+3", "2+3", true},
		{"separator first", "." + h1, h1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ContentHash(tc.version)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantOK, ok)
		})
	}
}

// TestContentHash_RoundTripsFormat: every form Format renders parses back to
// the hash it was rendered from.
func TestContentHash_RoundTripsFormat(t *testing.T) {
	for _, revision := range []int64{0, 1, 9_223_372_036_854_775_807} {
		for _, dirty := range []bool{false, true} {
			got, ok := ContentHash(Format(revision, dirty, h1))
			assert.True(t, ok)
			assert.Equal(t, h1, got)
		}
	}
}

// TestCompare covers the comparisons both sides make: the server's resume
// classification (Snapshot.resumeLocked: token vs its current version) and the
// agent's emptyResend (its presented token vs the marker's version).
func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		name           string
		token, current string
		want           Relation
	}{
		{"identical clean", "42." + h1, "42." + h1, Same},
		{"identical dirty", "42+" + h1, "42+" + h1, Same},
		{"identical fallback", "hash:" + h1, "hash:" + h1, Same},
		{"identical legacy counter", "17", "17", Same},
		{"both empty", "", "", Same},

		{"same hash, older revision", "41." + h1, "42." + h1, Renamed},
		{"same hash, clean vs dirty", "42." + h1, "42+" + h1, Renamed},
		{"same hash, dirty vs clean", "42+" + h1, "43." + h1, Renamed},
		{"same hash, fallback vs revisioned", "hash:" + h1, "42." + h1, Renamed},
		{"same hash, revisioned vs fallback", "42+" + h1, "hash:" + h1, Renamed},
		{"same hash, rebuilt etcd (revision went back)", "9000." + h1, "1." + h1, Renamed},

		{"other hash, same revision", "42." + h2, "42." + h1, Different},
		{"other hash, fallback", "hash:" + h2, "hash:" + h1, Different},
		{"other hash, mixed forms", "hash:" + h2, "42+" + h1, Different},
		{"legacy counter vs current", "17", "42." + h1, Different},
		{"current vs legacy counter", "42." + h1, "17", Different},
		{"two legacy counters", "17", "18", Different},
		{"empty token", "", "42." + h1, Different},
		{"empty current", "42." + h1, "", Different},
		{"garbage token", "not-a-version", "42." + h1, Different},
		{"empty hash parts never rename", "41.", "42.", Different},
		{"empty fallback hashes never rename", "hash:", "42.", Different},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Compare(tc.token, tc.current))
		})
	}
}

func TestRelation_String(t *testing.T) {
	assert.Equal(t, "same", Same.String())
	assert.Equal(t, "renamed", Renamed.String())
	assert.Equal(t, "different", Different.String())
}

// TestContentHashValue: the value is the first 13 hex digits of the hash as an
// integer -- 52 bits, which a float64 (what Prometheus stores) holds exactly.
func TestContentHashValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hash   string
		want   int64
		wantOK bool
	}{
		{"top 13 digits", h1, 0x0123456789abc, true},
		{"other hash", h2, 0xfedcba9876543, true},
		{"dropped digits do not matter", "0123456789abcfff", 0x0123456789abc, true},
		{"all zero", "0000000000000000", 0, true},
		{"largest", "ffffffffffffffff", 1<<52 - 1, true},
		{"exactly 13 digits", "0123456789abc", 0x0123456789abc, true},
		{"upper case hex", "0123456789ABCDEF", 0x0123456789abc, true},
		{"too short", "0123456789ab", 0, false},
		{"empty", "", 0, false},
		{"not hex", "0123456789abXdef", 0, false},
		{"a sign is not a hex digit", "-123456789abcdef", 0, false},
		{"a version is not a hash", "42." + h1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ContentHashValue(tc.hash)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestContentHashValue_Float64Exact: every value survives the round trip
// through a float64, including the largest and its neighbour -- the property
// the 52-bit budget exists for. 2^53+1 is the first integer that does not.
func TestContentHashValue_Float64Exact(t *testing.T) {
	assert.Equal(t, 52, ValueBits)
	for _, h := range []string{h1, h2, "ffffffffffffffff", "ffffffffffffefff", "0000000000001000"} {
		v, ok := ContentHashValue(h)
		assert.True(t, ok)
		assert.GreaterOrEqual(t, v, int64(0))
		assert.Less(t, v, int64(1)<<ValueBits)
		assert.Equal(t, v, int64(float64(v)), "hash %s", h)
	}
	a, _ := ContentHashValue("ffffffffffffffff")
	b, _ := ContentHashValue("ffffffffffffefff")
	assert.NotEqual(t, float64(a), float64(b), "adjacent values stay distinct as float64")

	beyond := int64(1)<<53 + 1
	assert.NotEqual(t, beyond, int64(float64(beyond)), "the budget is real: 2^53+1 is not a float64")
}

// TestContentHashValue_FromVersion: every version form yields the same value
// through the one parser (ContentHash), so a reader holding only a version
// string can reproduce the gauge.
func TestContentHashValue_FromVersion(t *testing.T) {
	want, _ := ContentHashValue(h1)
	for _, version := range []string{Format(42, false, h1), Format(42, true, h1), Format(0, false, h1)} {
		h, ok := ContentHash(version)
		assert.True(t, ok, version)
		got, ok := ContentHashValue(h)
		assert.True(t, ok, version)
		assert.Equal(t, want, got, version)
	}
}
