package udspath

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const testUID = "0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1"

func TestResolveCSI(t *testing.T) {
	got, err := ResolveCSI(DefaultCSIRoot, testUID, "sockets", "sockets/app.sock")
	require.NoError(t, err)
	// The volume name selects the carrier; it never appears in the path.
	assert.Equal(t, "/run/aether/uds/0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1/app.sock", got)
}

// TestMaxFileLen pins the budget derivation the proposal and the docs quote:
// "/run/aether/uds/" (16) + a 36-byte UID + "/" leaves 54 bytes of the 107.
func TestMaxFileLen(t *testing.T) {
	assert.Equal(t, 54, MaxFileLen)
	assert.Len(t, testUID, PodUIDLen)
}

// TestResolveCSI_PathLength pins the AF_UNIX sun_path budget. Envoy rejects a
// Pipe address over the limit and NACKs the whole CDS update, so resolution must
// fail first (the caller then degrades that one pod to TCP). The budget is the
// FILE name's alone: a long volume name costs nothing.
func TestResolveCSI_PathLength(t *testing.T) {
	longVolume := strings.Repeat("v", 60)
	atLimit, err := ResolveCSI(DefaultCSIRoot, testUID, longVolume, longVolume+"/"+strings.Repeat("f", MaxFileLen))
	require.NoError(t, err)
	require.Len(t, atLimit, MaxPipePathLen)

	_, err = ResolveCSI(DefaultCSIRoot, testUID, longVolume, longVolume+"/"+strings.Repeat("f", MaxFileLen+1))
	require.Error(t, err)
	assert.Equal(t, ReasonPathTooLong, ReasonOf(err))
	assert.Contains(t, err.Error(), "AF_UNIX limit")

	// A longer --uds-csi-root shrinks the budget; the agent re-checks against
	// its own root.
	_, err = ResolveCSI("/var/run/aether/uds", testUID, "s", "s/"+strings.Repeat("f", MaxFileLen))
	assert.Equal(t, ReasonPathTooLong, ReasonOf(err))
}

// TestResolveCSI_Rejections pins the fail-closed contract: the request is
// attacker-influenced, so anything that is not exactly <csi-volume>/<segment>
// must fail — with the reason the resolve-failure counter reports.
func TestResolveCSI_Rejections(t *testing.T) {
	cases := map[string]struct {
		root, uid, csiVolume, request string
		reason                        Reason
	}{
		"no separator":         {DefaultCSIRoot, testUID, "s", "appsock", ReasonBadRequest},
		"empty request":        {DefaultCSIRoot, testUID, "s", "", ReasonBadRequest},
		"empty volume":         {DefaultCSIRoot, testUID, "s", "/app.sock", ReasonBadRequest},
		"volume traversal":     {DefaultCSIRoot, testUID, "s", "../app.sock", ReasonBadRequest},
		"dot volume":           {DefaultCSIRoot, testUID, "s", "./app.sock", ReasonBadRequest},
		"absolute smuggle":     {DefaultCSIRoot, testUID, "s", "//etc/passwd", ReasonBadRequest},
		"other volume":         {DefaultCSIRoot, testUID, "s", "other/app.sock", ReasonVolumeNotDeclared},
		"pod has no csi vol":   {DefaultCSIRoot, testUID, "", "s/app.sock", ReasonVolumeNotDeclared},
		"empty file":           {DefaultCSIRoot, testUID, "s", "s/", ReasonBadFile},
		"extra segment":        {DefaultCSIRoot, testUID, "s", "s/deep/app.sock", ReasonBadFile},
		"file traversal":       {DefaultCSIRoot, testUID, "s", "s/..", ReasonBadFile},
		"file dot":             {DefaultCSIRoot, testUID, "s", "s/.", ReasonBadFile},
		"NUL in file":          {DefaultCSIRoot, testUID, "s", "s/app\x00.sock", ReasonBadFile},
		"empty uid":            {DefaultCSIRoot, "", "s", "s/app.sock", ReasonNoUID},
		"uid traversal":        {DefaultCSIRoot, "..", "s", "s/app.sock", ReasonNoUID},
		"uid with separator":   {DefaultCSIRoot, "a/b", "s", "s/app.sock", ReasonNoUID},
		"relative uds root":    {"run/aether/uds", testUID, "s", "s/app.sock", ""},
		"over budget (55 B)":   {DefaultCSIRoot, testUID, "s", "s/" + strings.Repeat("x", 55), ReasonPathTooLong},
		"volume named, no csi": {DefaultCSIRoot, testUID, "", "uds/app.sock", ReasonVolumeNotDeclared},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveCSI(tc.root, tc.uid, tc.csiVolume, tc.request)
			require.Error(t, err)
			assert.Equal(t, tc.reason, ReasonOf(err))
		})
	}
}

func TestValidateRequest(t *testing.T) {
	require.NoError(t, ValidateRequest("uds/app.sock"))
	require.NoError(t, ValidateRequest("a-very-long-volume-name-costs-nothing-now/"+strings.Repeat("f", MaxFileLen)))

	err := ValidateRequest("uds/" + strings.Repeat("f", MaxFileLen+1))
	assert.Equal(t, ReasonPathTooLong, ReasonOf(err))
	assert.Contains(t, err.Error(), "AF_UNIX")
	assert.Equal(t, ReasonBadFile, ReasonOf(ValidateRequest("uds/a/b")))
	assert.Equal(t, ReasonBadRequest, ReasonOf(ValidateRequest("nosep")))
}

func csiVol(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: CSIDriver}}}
}

func TestVolumesOf(t *testing.T) {
	emptyDir := corev1.Volume{Name: "e", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	spiffe := corev1.Volume{Name: "spiffe", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "csi.spiffe.io"}}}

	assert.Equal(t, PodVolumes{}, VolumesOf(nil))

	one := VolumesOf(&corev1.PodSpec{Volumes: []corev1.Volume{emptyDir, csiVol("uds"), spiffe}})
	assert.Equal(t, PodVolumes{CSIVolume: "uds", CSIVolumes: 1, Names: []string{"e", "uds", "spiffe"}}, one)

	// Another CSI driver is not the carrier.
	none := VolumesOf(&corev1.PodSpec{Volumes: []corev1.Volume{emptyDir, spiffe}})
	assert.Empty(t, none.CSIVolume)
	assert.Zero(t, none.CSIVolumes)

	// Two carriers: neither is trusted.
	two := VolumesOf(&corev1.PodSpec{Volumes: []corev1.Volume{csiVol("a"), csiVol("b")}})
	assert.Empty(t, two.CSIVolume)
	assert.Equal(t, uint32(2), two.CSIVolumes)
}

func TestResolvePod(t *testing.T) {
	vols := PodVolumes{CSIVolume: "uds", CSIVolumes: 1, Names: []string{"uds", "legacy"}}

	got, err := ResolvePod(DefaultCSIRoot, testUID, vols, "uds/a.sock")
	require.NoError(t, err)
	assert.Equal(t, DefaultCSIRoot+"/"+testUID+"/a.sock", got)

	// Declared, but not as the csi.aether.io volume: the cut-over's loud case.
	_, err = ResolvePod(DefaultCSIRoot, testUID, vols, "legacy/a.sock")
	assert.Equal(t, ReasonNotCSI, ReasonOf(err))
	assert.Contains(t, err.Error(), "csi: {driver: csi.aether.io}")
	assert.Contains(t, err.Error(), "fsGroup")

	// Not declared at all: drift.
	_, err = ResolvePod(DefaultCSIRoot, testUID, vols, "nope/a.sock")
	assert.Equal(t, ReasonVolumeNotDeclared, ReasonOf(err))

	// Other failures pass through unrefined.
	_, err = ResolvePod(DefaultCSIRoot, testUID, vols, "uds/"+strings.Repeat("x", 60))
	assert.Equal(t, ReasonPathTooLong, ReasonOf(err))

	// Two carriers: neither is trusted, and the reason says so even for a
	// request naming one of them.
	two := PodVolumes{CSIVolumes: 2, Names: []string{"a", "b"}}
	_, err = ResolvePod(DefaultCSIRoot, testUID, two, "a/x.sock")
	assert.Equal(t, ReasonMultipleCSIVolumes, ReasonOf(err))

	// A record written before 039 Phase 2 carries no volume data: a request
	// then reads as an undeclared volume, never as a resolvable path.
	_, err = ResolvePod(DefaultCSIRoot, testUID, PodVolumes{}, "uds/a.sock")
	assert.Equal(t, ReasonVolumeNotDeclared, ReasonOf(err))
}

func TestReasonsAreDistinct(t *testing.T) {
	seen := map[Reason]bool{}
	for _, r := range Reasons {
		assert.NotEmpty(t, r)
		assert.False(t, seen[r], "duplicate reason %q", r)
		seen[r] = true
	}
	for _, r := range []Reason{ReasonVolumeNotDeclared, ReasonNotCSI, ReasonBadFile, ReasonPathTooLong} {
		assert.True(t, seen[r], "reason %q is missing from Reasons (it would never be seeded)", r)
	}
}
