package meshdns

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookup(t *testing.T) {
	s := NewServer("aether.internal", "127.0.0.1:18054", slog.New(slog.DiscardHandler))
	// 020 Part 1: records keyed by "<ns>/<svc>".
	s.SetRecords(map[string]string{"team-a/svc-1": "10.111.0.5", "default/echo": "10.111.0.6"})

	// <svc>.<ns>.<meshDomain> -> its record; case-insensitive; trailing dot tolerated.
	assert.Equal(t, "10.111.0.5", s.lookup("svc-1.team-a.aether.internal."))
	assert.Equal(t, "10.111.0.5", s.lookup("SVC-1.TEAM-A.AETHER.INTERNAL"))
	assert.Equal(t, "10.111.0.6", s.lookup("echo.default.aether.internal."))

	// not answered -> forwarded (lookup returns ""):
	assert.Empty(t, s.lookup("svc-1.aether-test.svc.cluster.local."), "cluster.local is not a mesh name")
	assert.Empty(t, s.lookup("unknown.team-a.aether.internal."), "not in the record table")
	assert.Empty(t, s.lookup("svc-1.aether.internal."), "single label (no namespace) is not a mesh name now")
	assert.Empty(t, s.lookup("a.b.c.aether.internal."), "three labels under the mesh domain")
	assert.Empty(t, s.lookup("aether.internal."), "the bare mesh domain")
	assert.Empty(t, s.lookup("google.com."), "external name")
}
