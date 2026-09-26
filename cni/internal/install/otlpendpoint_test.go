package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/cni/conflist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const collectorName = "otel-collector.o11y.svc.cluster.local"

// fakeLookup answers from a fixed table and records every host it was asked for.
type fakeLookup struct {
	answers map[string][]string
	err     error
	asked   []string
}

func (f *fakeLookup) lookup(_ context.Context, host string) ([]string, error) {
	f.asked = append(f.asked, host)
	if f.err != nil {
		return nil, f.err
	}
	addrs, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

// TestPinOTLPEndpoint covers issue #950: the CNI plugin runs under the host's
// resolver, which cannot resolve a cluster Service name, so cni-install — which
// runs in a pod and CAN — writes the Service's address instead of its name.
func TestPinOTLPEndpoint(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		endpoint string
		answers  map[string][]string
		err      error
		want     string
		// asked is the host the resolver must have been queried for; empty
		// means it must not have been queried at all.
		asked string
	}{
		{
			name:     "a Service name is pinned to its ClusterIP",
			endpoint: collectorName + ":4317",
			answers:  map[string][]string{collectorName: {"10.96.12.34"}},
			want:     "10.96.12.34:4317",
			asked:    collectorName,
		},
		{
			name:     "an IPv6-only ClusterIP is bracketed",
			endpoint: collectorName + ":4317",
			answers:  map[string][]string{collectorName: {"fd00:10:96::1234"}},
			want:     "[fd00:10:96::1234]:4317",
			asked:    collectorName,
		},
		{
			// Dual-stack (or a headless Service): the choice must not depend on
			// the order DNS happened to answer in, or every agent start could
			// chain a different address.
			name:     "several addresses pin deterministically, IPv4 first",
			endpoint: collectorName + ":4317",
			answers:  map[string][]string{collectorName: {"fd00::9", "10.96.0.20", "10.96.0.3"}},
			want:     "10.96.0.3:4317",
			asked:    collectorName,
		},
		{
			name:     "a resolution failure keeps the name (pre-#950 behaviour)",
			endpoint: collectorName + ":4317",
			err:      errors.New("i/o timeout"),
			want:     collectorName + ":4317",
			asked:    collectorName,
		},
		{
			name:     "an empty answer keeps the name",
			endpoint: collectorName + ":4317",
			answers:  map[string][]string{collectorName: {}},
			want:     collectorName + ":4317",
			asked:    collectorName,
		},
		{
			name:     "an IP endpoint is left alone and never resolved",
			endpoint: "10.96.0.7:4317",
			want:     "10.96.0.7:4317",
		},
		{
			name:     "a bracketed IPv6 endpoint is left alone and never resolved",
			endpoint: "[fd00::7]:4317",
			want:     "[fd00::7]:4317",
		},
		{
			name:     "an empty endpoint (telemetry off) stays empty",
			endpoint: "",
			want:     "",
		},
		{
			name:     "an endpoint without a port is left alone",
			endpoint: collectorName,
			want:     collectorName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLookup{answers: tc.answers, err: tc.err}
			got := pinOTLPEndpoint(ctx, discardLogger(), tc.endpoint, f.lookup)
			assert.Equal(t, tc.want, got)
			if tc.asked == "" {
				assert.Empty(t, f.asked, "the resolver must not be queried")
			} else {
				assert.Equal(t, []string{tc.asked}, f.asked)
			}
		})
	}
}

// renderedOTLPEndpoint runs the real installer render against a flannel-shaped
// conflist and returns the otlp_endpoint that ended up chained on disk.
func renderedOTLPEndpoint(t *testing.T, cfg *InstallerConfig) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "10-flannel.conflist", `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)
	cfg.MountedCNINetDir = dir

	_, err := createCNIConfigFile(context.Background(), discardLogger(), cfg)
	require.NoError(t, err)

	merged, err := os.ReadFile(filepath.Join(dir, "10-flannel.conflist"))
	require.NoError(t, err)
	chain, err := conflist.Parse(merged)
	require.NoError(t, err)
	entry, present, err := chain.AetherEntry()
	require.NoError(t, err)
	require.True(t, present)

	var got struct {
		OTLPEndpoint string `json:"otlp_endpoint"`
	}
	require.NoError(t, json.Unmarshal(entry, &got))
	return got.OTLPEndpoint
}

// TestCreateCNIConfigFilePinsTheOTLPEndpoint is the netconf-level gate for
// #950: what the plugin reads off disk is the address, not the name.
func TestCreateCNIConfigFilePinsTheOTLPEndpoint(t *testing.T) {
	answers := map[string][]string{collectorName: {"10.96.12.34"}}

	t.Run("pinning on writes the ClusterIP", func(t *testing.T) {
		f := &fakeLookup{answers: answers}
		got := renderedOTLPEndpoint(t, &InstallerConfig{
			OTLPEndpoint:    collectorName + ":4317",
			PinOTLPEndpoint: true,
			lookupHost:      f.lookup,
		})
		assert.Equal(t, "10.96.12.34:4317", got)
	})

	t.Run("pinning off writes the name verbatim", func(t *testing.T) {
		f := &fakeLookup{answers: answers}
		got := renderedOTLPEndpoint(t, &InstallerConfig{
			OTLPEndpoint:    collectorName + ":4317",
			PinOTLPEndpoint: false,
			lookupHost:      f.lookup,
		})
		assert.Equal(t, collectorName+":4317", got)
		assert.Empty(t, f.asked, "pinning off must not resolve")
	})

	t.Run("an unresolvable name is written verbatim", func(t *testing.T) {
		f := &fakeLookup{err: errors.New("no such host")}
		got := renderedOTLPEndpoint(t, &InstallerConfig{
			OTLPEndpoint:    collectorName + ":4317",
			PinOTLPEndpoint: true,
			lookupHost:      f.lookup,
		})
		assert.Equal(t, collectorName+":4317", got)
	})
}
