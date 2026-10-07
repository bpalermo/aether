package envoy_validate

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpAccessLogName is the log_name of the per-request stream (the unexported
// accessLogName in agent/internal/xds/proxy).
const httpAccessLogName = "aether_access_logs"

// TestGeneratedAccessLogsCarryGenerationAndTiming reads the SERIALISED
// bootstraps -- the bytes TestEnvoyValidate hands to `envoy --mode validate` --
// and pins the aether#1333 fields on every access logger in them: the five
// per-request fields on each HTTP connection manager's logger (both reporters:
// the mTLS and HTTP/3 inbound, the outbound and the capture listener), and
// proxy_epoch on each capture tcp_proxy's connection-level logger.
//
// Presence in the generated config is the claim here. Validate mode accepts
// these strings, and would equally accept a misspelt time point; rendering is
// //agent/test/mtlspool TestAccessLogConnectionTiming.
func TestGeneratedAccessLogsCarryGenerationAndTiming(t *testing.T) {
	const epoch = "%ENVIRONMENT(AETHER_RESTART_EPOCH)%"
	wantHTTP := map[string]string{
		"proxy_epoch":   epoch,
		"connection_id": "%CONNECTION_ID%",
		"ds_cx_age_ms":  "%COMMON_DURATION(DS_CX_BEG:DS_RX_BEG:ms)%",
		"ds_hs_ms":      "%COMMON_DURATION(DS_CX_BEG:DS_HS_END:ms)%",
		"us_tx_beg_ms":  "%COMMON_DURATION(DS_RX_BEG:US_TX_BEG:ms)%",
	}
	fixtures := []struct {
		name string
		fn   func() ([]byte, error)
		// How many loggers of each stream the fixture must carry, so the check
		// cannot pass on a fixture that lost its access logs.
		http, l4 int
	}{
		// mTLS inbound, HTTP/3 inbound, outbound.
		{"node_bootstrap.json", NodeBootstrapJSON, 3, 0},
		{"capture_bootstrap.json", CaptureBootstrapJSON, 1, 0},
		{"capture_tcproute_bootstrap.json", CaptureTCPRouteBootstrapJSON, 0, 1},
		{"capture_tlsroute_bootstrap.json", CaptureTLSRouteBootstrapJSON, 0, 1},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			data, err := fx.fn()
			require.NoError(t, err)
			logs, err := AccessLogFormats(data)
			require.NoError(t, err)
			nHTTP, nL4 := 0, 0
			for _, l := range logs {
				switch l.LogName {
				case httpAccessLogName:
					nHTTP++
					for key, want := range wantHTTP {
						assert.Equalf(t, want, l.Attributes[key], "%s: %s", l.Where, key)
					}
					assert.Containsf(t, []string{proxy.ReporterSource, proxy.ReporterDestination}, l.Attributes["reporter"], "%s: reporter", l.Where)
				case proxy.L4AccessLogName:
					nL4++
					assert.Equalf(t, epoch, l.Attributes["proxy_epoch"], "%s: proxy_epoch", l.Where)
					for _, key := range []string{"connection_id", "ds_cx_age_ms", "ds_hs_ms", "us_tx_beg_ms"} {
						assert.NotContainsf(t, l.Attributes, key, "%s: a request-anchored field on a connection-level record", l.Where)
					}
				default:
					t.Errorf("%s: an access logger on a stream this test does not know (%q): say which fields it must carry", l.Where, l.LogName)
				}
			}
			assert.GreaterOrEqualf(t, nHTTP, fx.http, "HTTP access loggers examined (found %d in %d loggers)", nHTTP, len(logs))
			assert.GreaterOrEqualf(t, nL4, fx.l4, "L4 access loggers examined (found %d in %d loggers)", nL4, len(logs))
		})
	}
}

// TestAccessLogFormatsRejectsGarbage: a bootstrap that does not parse is an
// error, not an empty (and therefore passing) list.
func TestAccessLogFormatsRejectsGarbage(t *testing.T) {
	_, err := AccessLogFormats([]byte("{not json"))
	require.Error(t, err)
}
