package proxy

import (
	"strings"
	"testing"
)

// TestUnsupportedUDPRouteShapes pins the UDPRoute inputs the per-pod UDP
// capture listener cannot represent (#873), and -- since proposal 038 gave the
// listener a per-VIP matcher -- the one it now CAN: a second UDPRoute-backed
// service on the same pod.
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
		noVIP   bool // leave every parent without a ClusterIP
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
			// 038: the matcher keys on the dialled VIP, so a second parent is an
			// ordinary second arm. This used to be "dropped entirely".
			name: "second service is a second arm, not a discard",
			routes: map[string][]L4Backend{
				"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}},
				"ns/b": {{Cluster: "udp:b.mesh", Weight: 1}},
			},
			want: 0,
		},
		{
			// The arm needs the parent's VIP; until the mesh Service is observed
			// there is nothing to key on, and that must be said rather than
			// silently building no arm.
			name:    "a parent with no known ClusterIP reports",
			routes:  map[string][]L4Backend{"ns/novip": {{Cluster: "udp:novip.mesh", Weight: 1}}},
			noVIP:   true,
			want:    1,
			contain: "no known ClusterIP",
		},
		{
			// #873 changed what this reports, not whether it reports. The drain
			// is now HONOURED (the listener no longer forwards to a drained
			// backend), and because it was this service's only backend the
			// service ends up contributing no data path at all — still worth
			// saying out loud, since the UDPRoute was accepted.
			name: "weight 0 drain leaves the service with nothing to route to",
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
			// #873: this case USED to report 3 (weights discarded + drain not
			// honoured + second service dropped). The drain on udp:a1.mesh is
			// honoured, which leaves ns/a with exactly ONE routable backend, and
			// 038 made the second service an arm of its own. Nothing left to
			// report.
			name: "every shape at once",
			routes: map[string][]L4Backend{
				"ns/a": {
					{Cluster: "udp:a1.mesh", Weight: 0},
					{Cluster: "udp:a2.mesh", Weight: 5},
				},
				"ns/b": {{Cluster: "udp:b.mesh", Weight: 1}},
			},
			want: 0,
		},
		{
			// The drain is honoured, so the SURVIVING count is what decides
			// whether weights were discarded: three backends, one drained, two
			// left to split between and no way to express the split.
			name: "weights are still discarded among the backends that survive the drain",
			routes: map[string][]L4Backend{
				"ns/a": {
					{Cluster: "udp:a1.mesh", Weight: 0},
					{Cluster: "udp:a2.mesh", Weight: 5},
					{Cluster: "udp:a3.mesh", Weight: 95},
				},
			},
			want:    1,
			contain: "the backend weights are discarded",
		},
		{
			// A backend with no resolved cluster cannot be bound either, and a
			// service left with none of them is in the same place as a fully
			// drained one: the route is accepted and produces no data path.
			name: "a service with no resolvable backend reports",
			routes: map[string][]L4Backend{
				"ns/a": {{Cluster: "", Weight: 7}},
			},
			want:    1,
			contain: "contributes no UDP route",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vips := map[string]string{}
			if !tt.noVIP {
				for svc := range tt.routes {
					vips[svc] = syntheticVIP(svc)
				}
			}
			got := UnsupportedUDPRouteShapes(tt.routes, vips)
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

// TestUnsupportedUDPRouteShapesTracksTheGenerator is the anti-drift check.
//
// #874 shipped the detector as a deliberate DUPLICATE of the generator's
// selection, pinned by this test, because a detector that drifts confidently
// names the wrong cluster and is worse than no warning at all. #873 removed the
// duplicate: both now call selectUDPCaptureArms, so the drift is unrepresentable
// rather than merely tested for. The test stays as the end-to-end pin — it is
// what would catch a future refactor that gives either side its own copy again.
//
// The shape: two parents, one with a VIP and one without. The detector must
// name the VIP-less one as unrepresented, and the generator must carry exactly
// the other as an arm.
func TestUnsupportedUDPRouteShapesTracksTheGenerator(t *testing.T) {
	routes := map[string][]L4Backend{
		"ns/z": {{Cluster: "udp:z.mesh", Weight: 1}},
		"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}},
	}
	vips := map[string]string{"ns/a": "10.96.0.10"} // ns/z has none yet

	got := UnsupportedUDPRouteShapes(routes, vips)
	if len(got) != 1 {
		t.Fatalf("got %d reason(s), want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"ns/z"`) || !strings.Contains(got[0], "no known ClusterIP") {
		t.Fatalf("reason should say ns/z has no ClusterIP, got: %s", got[0])
	}

	l, err := GenerateUDPCaptureListener("pod-1", "/var/run/netns/x", 18082, routes, vips)
	if err != nil {
		t.Fatalf("GenerateUDPCaptureListener: %v", err)
	}
	if l == nil {
		t.Fatal("expected a listener")
	}
	// The generator really did carry the one the detector did NOT name, and
	// not the one it did.
	if !strings.Contains(l.String(), "udp:a.mesh") {
		t.Fatalf("generator did not bind udp:a.mesh; detector and generator have drifted:\n%s", l.String())
	}
	if strings.Contains(l.String(), "udp:z.mesh") {
		t.Fatalf("generator bound udp:z.mesh with no VIP to key it on; detector and generator have drifted:\n%s", l.String())
	}
}
