package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClampRefusalBody: a refusal's body is at most what Envoy accepts in a
// direct response (4096 bytes: a longer one has the WHOLE route table
// rejected). One that fits is untouched; a longer one is cut at a character
// boundary and ends in a mark that says so; and the refusal that is built
// carries the cut body.
func TestClampRefusalBody(t *testing.T) {
	assert.Equal(t, 4096, MaxRefusalBody, "Envoy's default max_direct_response_body_size_bytes")
	const mark = " ... (truncated)\n"
	for _, n := range []int{0, 1, MaxRefusalBody - 1, MaxRefusalBody} {
		body := strings.Repeat("a", n)
		assert.Equalf(t, body, ClampRefusalBody(body), "%d bytes fit", n)
	}
	for _, n := range []int{MaxRefusalBody + 1, MaxRefusalBody + 2, 3 * MaxRefusalBody} {
		got := ClampRefusalBody(strings.Repeat("a", n))
		assert.Lenf(t, got, MaxRefusalBody, "%d bytes", n)
		assert.Equal(t, strings.Repeat("a", MaxRefusalBody-len(mark))+mark, got)
	}
	// A three-byte character across the cut, at each of its offsets: it is
	// left out whole, never cut in two.
	for lead := 0; lead < 3; lead++ {
		body := strings.Repeat("a", lead) + strings.Repeat("€", 2*MaxRefusalBody)
		got := ClampRefusalBody(body)
		require.Truef(t, utf8.ValidString(got), "lead %d: a character was cut in two", lead)
		assert.LessOrEqual(t, len(got), MaxRefusalBody)
		assert.Greater(t, len(got), MaxRefusalBody-3, "no more is left out than the character that did not fit")
		assert.True(t, strings.HasSuffix(got, "€"+mark))
		assert.Equal(t, got, ClampRefusalBody(body), "the same body is cut the same")
	}

	long := strings.Repeat("port ", 2000)
	vh := BuildRefusalVirtualHost("svc|refused", []string{"svc.ns"}, RefusalPortNotDeclared, long)
	assert.Equal(t, ClampRefusalBody(long), vh.GetRoutes()[0].GetDirectResponse().GetBody().GetInlineString())
}
