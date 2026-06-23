package meshdns

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookup(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:18054", slog.New(slog.DiscardHandler))
	s.SetRecords(map[string]string{"svc-1": "10.111.0.5", "echo": "10.111.0.6"})

	// <svc>.<meshDomain> -> its record; case-insensitive; trailing dot tolerated.
	assert.Equal(t, "10.111.0.5", s.lookup("svc-1.aether.internal."))
	assert.Equal(t, "10.111.0.5", s.lookup("SVC-1.AETHER.INTERNAL"))
	assert.Equal(t, "10.111.0.6", s.lookup("echo.aether.internal."))

	// not answered -> forwarded (lookup returns ""):
	assert.Empty(t, s.lookup("svc-1.aether-test.svc.cluster.local."), "cluster.local is not a mesh name")
	assert.Empty(t, s.lookup("unknown.aether.internal."), "not in the record table")
	assert.Empty(t, s.lookup("a.b.aether.internal."), "nested label under the mesh domain")
	assert.Empty(t, s.lookup("aether.internal."), "the bare mesh domain")
	assert.Empty(t, s.lookup("google.com."), "external name")
}
