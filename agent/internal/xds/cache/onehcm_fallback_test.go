package cache

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const tableCompileFailuresCtr = "aether.agent.capture.table_compile_failures"

// When the per-port tables cannot be compiled into cap_http (a construct the
// compiler does not model; no builder makes one), the cache publishes
// RefuseCaptureMeshNames under the name: every mesh name and short name is
// refused 421 port-not-declared on every dialled port, none is delivered, and
// the failure is counted. Not parallel: it swaps the package's compiler.
func TestOneHCM_ACompileFailurePublishesTheRefusalTableAndCountsIt(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	reader := sdkmetric.NewManualReader()
	m, err := cachemetrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	c.metrics = m
	require.NoError(t, c.generateSnapshot(context.Background()))
	require.Zero(t, counterValue(t, reader, tableCompileFailuresCtr), "the real tables compile")
	require.NotEqual(t, "capture_table_unbuilt", xdseval.PickVirtualHost(a043Published(t, c), "multi.demo."+a043Mesh+":8080").GetName())

	restore := mergeCaptureTables
	t.Cleanup(func() { mergeCaptureTables = restore })
	mergeCaptureTables = func(string, map[uint32]*routev3.RouteConfiguration, *routev3.RouteConfiguration) (*routev3.RouteConfiguration, error) {
		return nil, errors.New("forced")
	}
	c.snapshotMu.Lock()
	c.captureTables.valid = false
	c.snapshotMu.Unlock()
	require.NoError(t, c.generateSnapshot(context.Background()))

	assert.Equal(t, int64(1), counterValue(t, reader, tableCompileFailuresCtr))
	published := a043Published(t, c)
	table := xdseval.NewTable(published)
	checked := 0
	for _, s := range services {
		mesh, kube := a043Names(s, a043BareIsUnique(services, s))
		for _, name := range append([]string{mesh, s.name}, kube...) {
			for _, suffix := range []string{"", ":80", ":8080", ":9090"} {
				for _, dialled := range []string{"80", "8080", "9090", strconv.Itoa(otherDialled), ""} {
					fs := map[string]string{proxy.CallerNamespaceFilterStateKey: s.ns}
					if dialled != "" {
						fs[proxy.DialledPortFilterStateKey] = dialled
					}
					req := xdseval.Request{Host: name + suffix, FilterState: fs}
					a, err := table.Resolve(req)
					require.NoError(t, err)
					require.False(t, a.NoVhost, "%s", req.Host)
					first, err := xdseval.FirstRoute(a.Routes, req)
					require.NoError(t, err)
					require.NotNil(t, first, "%s", req.Host)
					o := xdseval.OutcomeOf(first)
					assert.Equal(t, proxy.RefusalPortNotDeclared, o.Reason, "%s dialled %q: refused, never delivered (%+v)", req.Host, dialled, o)
					checked++
				}
			}
		}
	}
	require.NotZero(t, checked)
}
