package meshdns

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNameserversFromResolvConf(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(p, []byte(
		"# comment\nsearch aether-test.svc.cluster.local svc.cluster.local\nnameserver 10.96.0.10\nnameserver 10.96.0.11\noptions ndots:2\n",
	), 0o644))
	assert.Equal(t, []string{"10.96.0.10", "10.96.0.11"}, NameserversFromResolvConf(p))
	assert.Nil(t, NameserversFromResolvConf(filepath.Join(dir, "missing")))
}
