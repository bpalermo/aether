package v1

import (
	"encoding/json"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEdgeConfig_EnumCanonicalName verifies the jsonshim parses the CANONICAL
// (buf ENUM_VALUE_PREFIX) enum value name the chart writes into the default
// EdgeConfig CR. Regression guard: after prefixing the enum values, the short form
// ("REJECT_REQUEST") no longer parses via protojson — the chart + CRs must use the
// full "HEADERS_WITH_UNDERSCORES_ACTION_REJECT_REQUEST".
func TestEdgeConfig_EnumCanonicalName(t *testing.T) {
	raw := `{"apiVersion":"config.aether.io/v1","kind":"EdgeConfig","metadata":{"name":"d"},"spec":{"useRemoteAddress":true,"headersWithUnderscoresAction":"HEADERS_WITH_UNDERSCORES_ACTION_REJECT_REQUEST"}}`
	var ec EdgeConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &ec), "canonical enum name must parse through the jsonshim")
	assert.Equal(t, configv1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_REJECT_REQUEST, ec.Spec.GetHeadersWithUnderscoresAction())
	assert.True(t, ec.Spec.GetUseRemoteAddress().GetValue())

	// The short (unprefixed) form is the PITFALL: the jsonshim's protojson
	// DiscardUnknown does NOT error on an unrecognized enum string — it SILENTLY
	// drops it to UNSPECIFIED. So a chart/CR using the short "ALLOW"/"REJECT_REQUEST"
	// is silently ignored (falls to the compiled default, not the author's intent).
	// This is why the chart must emit the canonical prefixed name.
	short := `{"spec":{"headersWithUnderscoresAction":"ALLOW"}}`
	var dropped EdgeConfig
	require.NoError(t, json.Unmarshal([]byte(short), &dropped))
	assert.Equal(t, configv1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_UNSPECIFIED,
		dropped.Spec.GetHeadersWithUnderscoresAction(),
		"short enum form is silently dropped to UNSPECIFIED — the author's ALLOW is lost")
}
