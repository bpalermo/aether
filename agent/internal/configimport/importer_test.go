package configimport

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeImporter struct {
	projections []*registryv1.ServiceConfigProjection
	err         error
}

func (f *fakeImporter) ListConfig(context.Context) ([]*registryv1.ServiceConfigProjection, error) {
	return f.projections, f.err
}

type fakeSink struct{ routes map[string][]proxy.GammaRoute }

func (s *fakeSink) SetImportedServiceRoutes(r map[string][]proxy.GammaRoute) { s.routes = r }

func newImporter(t *testing.T, imp *fakeImporter, sink *fakeSink, own string) *Importer {
	t.Helper()
	return NewImporter(imp, sink, own, 0, slog.New(slog.DiscardHandler))
}

func proj(svc, origin, version, prefix string) *registryv1.ServiceConfigProjection {
	return &registryv1.ServiceConfigProjection{
		Service: svc, OriginCluster: origin, Version: version,
		Routes: []*registryv1.GammaRoute{{Matches: []*registryv1.GammaMatch{{Prefix: prefix}}}},
	}
}

// TestImporter_Materialize_SkipsOwnAndPicksHighestVersion verifies the consumer skips
// its own exports and, on a same-service peer conflict, keeps the highest version.
func TestImporter_Materialize_SkipsOwnAndPicksHighestVersion(t *testing.T) {
	imp := &fakeImporter{projections: []*registryv1.ServiceConfigProjection{
		proj("team-a/echo", "self", "v9", "/own"),   // own cluster — skipped
		proj("team-a/echo", "peer-a", "v1", "/old"), // peer, older
		proj("team-a/echo", "peer-b", "v2", "/new"), // peer, newer → wins
		proj("team-b/svc", "peer-a", "v1", "/svc"),
	}}
	sink := &fakeSink{}
	newImporter(t, imp, sink, "self").poll(context.Background())

	require.Len(t, sink.routes, 2)
	require.Contains(t, sink.routes, "team-a/echo")
	assert.Equal(t, "/new", sink.routes["team-a/echo"][0].Matches[0].Prefix, "highest version wins; own export skipped")
	require.Contains(t, sink.routes, "team-b/svc")
}

// TestImporter_Poll_KeepsLastKnownOnError verifies a fetch error does not clobber the
// cache (AP — last-known retained; the sink is simply not called).
func TestImporter_Poll_KeepsLastKnownOnError(t *testing.T) {
	sink := &fakeSink{routes: map[string][]proxy.GammaRoute{"x": nil}}
	imp := &fakeImporter{err: assert.AnError}
	newImporter(t, imp, sink, "self").poll(context.Background())
	assert.Equal(t, map[string][]proxy.GammaRoute{"x": nil}, sink.routes, "error must not overwrite last-known imported routes")
}

// TestImporter_Materialize_Empty verifies an all-own / empty projection set yields nil
// (no imported routes).
func TestImporter_Materialize_Empty(t *testing.T) {
	imp := &fakeImporter{projections: []*registryv1.ServiceConfigProjection{proj("a/b", "self", "v1", "/")}}
	sink := &fakeSink{}
	newImporter(t, imp, sink, "self").poll(context.Background())
	assert.Nil(t, sink.routes)
}
