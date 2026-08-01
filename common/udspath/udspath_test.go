package udspath

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	got, err := Resolve("/var/lib/kubelet/pods", "0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1", "sockets/app.sock")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/kubelet/pods/0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1/volumes/kubernetes.io~empty-dir/sockets/app.sock", got)
}

// TestResolve_Rejections pins the fail-closed contract: the annotation is
// attacker-influenced, so anything that is not exactly <segment>/<segment>
// must fail, never escape the pod's own emptyDir directory.
func TestResolve_Rejections(t *testing.T) {
	const (
		podsDir = "/var/lib/kubelet/pods"
		uid     = "0f52c50e-99cf-4a3c-a5e3-6a1e60e2b5f1"
	)
	cases := map[string]struct {
		podsDir, uid, annotation string
	}{
		"no separator":              {podsDir, uid, "appsock"},
		"empty annotation":          {podsDir, uid, ""},
		"empty volume":              {podsDir, uid, "/app.sock"},
		"empty file":                {podsDir, uid, "sockets/"},
		"extra segment":             {podsDir, uid, "sockets/deep/app.sock"},
		"volume traversal":          {podsDir, uid, "../app.sock"},
		"file traversal":            {podsDir, uid, "sockets/.."},
		"dot volume":                {podsDir, uid, "./app.sock"},
		"absolute smuggle":          {podsDir, uid, "//etc/passwd"},
		"NUL in file":               {podsDir, uid, "sockets/app\x00.sock"},
		"empty uid":                 {podsDir, "", "sockets/app.sock"},
		"uid traversal":             {podsDir, "..", "sockets/app.sock"},
		"uid with separator":        {podsDir, "a/b", "sockets/app.sock"},
		"relative kubelet pods dir": {"var/lib/kubelet/pods", uid, "sockets/app.sock"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(tc.podsDir, tc.uid, tc.annotation)
			assert.Error(t, err)
		})
	}
}
