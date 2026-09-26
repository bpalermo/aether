package mesh

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// meshSources is this package's own non-test source, embedded so the scan works
// inside Bazel's sandbox where the test cwd holds no sources. The same
// mechanism as agent/internal/xds/proxy's TestEastWestPortIsSharedAcrossTransports.
//
//go:embed *.go
var meshSources embed.FS

// TestL4OutboundPortIsSharedAcrossTransports is the port-role gate for the L4
// mesh spelling (proposal 038 D1 / R3): plaintext UDP dials the SAME number as
// raw TCP — ProxyL4OutboundPort, 18082 — never a new port. The east-west
// gateway has the same gate on 18009; the inbound gains one on 18008 with the
// QUIC listener.
//
// It scans this package's constants for any name that says "outbound" and a
// transport ("udp", "quic", "h3", "http3") and "port", and requires its value to
// equal ProxyL4OutboundPort. Today no such constant exists, and that is the
// point: green now, red on the exact commit that would allocate `ProxyUDPOutboundPort
// = 18083`, with the offending name in the message. A written constraint with
// no gate is a wish (#853).
func TestL4OutboundPortIsSharedAcrossTransports(t *testing.T) {
	outbound := regexp.MustCompile(`(?i)outbound`)
	transport := regexp.MustCompile(`(?i)(udp|quic|h3|http3)`)
	port := regexp.MustCompile(`(?i)port`)

	entries, err := meshSources.ReadDir(".")
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, e.Name())
		}
	}
	require.NotEmpty(t, files, "the embed must see this package's sources, or the scan is vacuous")

	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		src, err := meshSources.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err, f)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					n := name.Name
					if !(outbound.MatchString(n) && transport.MatchString(n) && port.MatchString(n)) {
						continue
					}
					found++
					require.Less(t, i, len(vs.Values), "%s: transport-specific outbound port constant with no literal value", n)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok, "%s: outbound port constants must be integer literals so this test can read them", n)
					v, err := strconv.Atoi(lit.Value)
					require.NoError(t, err, n)
					assert.Equal(t, ProxyL4OutboundPort, v,
						"%s = %d: plaintext UDP/QUIC dial the SAME L4 spelling as raw TCP (%d); a second number is a second CNI rule, Service port and capture path (038 D1)",
						n, v, ProxyL4OutboundPort)
				}
			}
		}
	}

	// Anti-vacuity: the scan must have seen the constant it guards.
	src, err := meshSources.ReadFile("mesh.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "ProxyL4OutboundPort = "+strconv.Itoa(ProxyL4OutboundPort),
		"the constant this test guards must be defined in mesh.go with a literal value")
	t.Logf("transport-specific outbound port constants found: %d (0 is the expected steady state)", found)
}
