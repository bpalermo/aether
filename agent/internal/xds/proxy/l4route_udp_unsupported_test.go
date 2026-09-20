package proxy

import (
	"strings"
	"testing"
)

// TestUnsupportedUDPRouteShapes pins the three UDPRoute inputs the per-pod UDP
// capture listener cannot represent (#873).
//
// The point of the function is that these discards are otherwise INVISIBLE: the
// route is accepted, common/l4project weights it correctly, and the data plane
// quietly ignores it. So the cases that must report something matter as much as
// the case that must stay silent — a detector that never fires and a detector
// with nothing to report look identical from the outside.
func TestUnsupportedUDPRouteShapes(t *testing.T) {
	tests := []struct {
		name    string
		routes  map[string][]L4Backend
		want    int
		contain string
	}{
		{
			name:   "no routes is silent",
			routes: map[string][]L4Backend{},
			want:   0,
		},
		{
			name: "single service single weighted backend is representable",
			routes: map[string][]L4Backend{
				"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}},
			},
			want: 0,
		},
		{
			name: "multiple backends discard the weights",
			routes: map[string][]L4Backend{
				"ns/a": {
					{Cluster: "udp:a1.mesh", Weight: 90},
					{Cluster: "udp:a2.mesh", Weight: 10},
				},
			},
			want:    1,
			contain: "weights are discarded",
		},
		{
			name: "second service is dropped entirely",
			routes: map[string][]L4Backend{
				"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}},
				"ns/b": {{Cluster: "udp:b.mesh", Weight: 1}},
			},
			want:    1,
			contain: "dropped entirely",
		},
		{
			name: "weight 0 drain is not honoured for UDP",
			routes: map[string][]L4Backend{
				"ns/a": {{Cluster: "udp:a.mesh", Weight: 0}},
			},
			want:    1,
			contain: "weight 0 (drain)",
		},
		{
			name: "empty backend lists are skipped, not chosen",
			routes: map[string][]L4Backend{
				"ns/a": {},
				"ns/b": {{Cluster: "udp:b.mesh", Weight: 1}},
			},
			want: 0,
		},
		{
			name: "every shape at once",
			routes: map[string][]L4Backend{
				"ns/a": {
					{Cluster: "udp:a1.mesh", Weight: 0},
					{Cluster: "udp:a2.mesh", Weight: 5},
				},
				"ns/b": {{Cluster: "udp:b.mesh", Weight: 1}},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnsupportedUDPRouteShapes(tt.routes)
			if len(got) != tt.want {
				t.Fatalf("got %d reason(s), want %d: %v", len(got), tt.want, got)
			}
			if tt.contain == "" {
				return
			}
			if !strings.Contains(strings.Join(got, "\n"), tt.contain) {
				t.Fatalf("no reason contained %q: %v", tt.contain, got)
			}
		})
	}
}

// TestUnsupportedUDPRouteShapesTracksTheGenerator is the anti-drift check. The
// detector duplicates GenerateUDPCaptureListener's selection (first non-empty
// service over SORTED keys, then backends[0]); if the generator's choice ever
// changes and the detector's does not, the warning would name the wrong cluster
// while the listener silently used another — worse than no warning.
func TestUnsupportedUDPRouteShapesTracksTheGenerator(t *testing.T) {
	// "ns/a" sorts before "ns/z", so the generator binds to a1 and the detector
	// must say z is the one dropped, not a.
	routes := map[string][]L4Backend{
		"ns/z": {{Cluster: "udp:z.mesh", Weight: 1}},
		"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}},
	}

	got := UnsupportedUDPRouteShapes(routes)
	if len(got) != 1 {
		t.Fatalf("got %d reason(s), want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"ns/z"`) || !strings.Contains(got[0], "udp:a.mesh") {
		t.Fatalf("reason should say ns/z was dropped because the listener is bound to udp:a.mesh, got: %s", got[0])
	}

	l, err := GenerateUDPCaptureListener("pod-1", "/var/run/netns/x", 18001, routes)
	if err != nil {
		t.Fatalf("GenerateUDPCaptureListener: %v", err)
	}
	if l == nil {
		t.Fatal("expected a listener")
	}
	// The generator really did bind to the service the detector named.
	if !strings.Contains(l.String(), "udp:a.mesh") {
		t.Fatalf("generator did not bind to udp:a.mesh; detector and generator have drifted:\n%s", l.String())
	}
}
