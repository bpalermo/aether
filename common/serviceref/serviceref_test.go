package serviceref

import "testing"

func TestServiceRefFormats(t *testing.T) {
	r := New("team-a", "echo")
	if got := r.Key(); got != "team-a/echo" {
		t.Errorf("Key() = %q, want team-a/echo", got)
	}
	if got := r.FQDN("aether.internal"); got != "echo.team-a.aether.internal" {
		t.Errorf("FQDN() = %q, want echo.team-a.aether.internal", got)
	}
	if got := r.ClusterLocalFQDN(); got != "echo.team-a.svc.cluster.local" {
		t.Errorf("ClusterLocalFQDN() = %q, want echo.team-a.svc.cluster.local", got)
	}
	if got := r.String(); got != "team-a/echo" {
		t.Errorf("String() = %q, want team-a/echo", got)
	}
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		in      string
		wantRef ServiceRef
		wantOK  bool
	}{
		{"team-a/echo", ServiceRef{Namespace: "team-a", Name: "echo"}, true},
		{"default/svc-1", ServiceRef{Namespace: "default", Name: "svc-1"}, true},
		{"echo", ServiceRef{}, false},    // namespace-free (the old key) must NOT parse
		{"/echo", ServiceRef{}, false},   // empty namespace
		{"team-a/", ServiceRef{}, false}, // empty name
		{"", ServiceRef{}, false},        // empty
		{"a/b/c", ServiceRef{}, false},   // ambiguous extra "/"
	}
	for _, tt := range tests {
		gotRef, gotOK := ParseKey(tt.in)
		if gotOK != tt.wantOK || gotRef != tt.wantRef {
			t.Errorf("ParseKey(%q) = (%+v, %v), want (%+v, %v)", tt.in, gotRef, gotOK, tt.wantRef, tt.wantOK)
		}
	}
}

// TestRoundTrip: Key() and ParseKey are inverses for valid refs.
func TestRoundTrip(t *testing.T) {
	for _, r := range []ServiceRef{New("default", "echo"), New("team-a", "svc-1")} {
		got, ok := ParseKey(r.Key())
		if !ok || got != r {
			t.Errorf("round-trip %+v -> %q -> (%+v, %v)", r, r.Key(), got, ok)
		}
	}
}
