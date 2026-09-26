package proxy

import (
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

// TestInboundPortIsSharedAcrossTransports is the port-role gate for the mesh
// inbound (proposal 038 R3): HTTP/3 inbound binds UDP on the SAME number as the
// TCP inbound, defaultInboundPort (18008), never a new port -- the east-west
// gateway (TestEastWestPortIsSharedAcrossTransports) and the L4 spelling
// (common/constants/mesh) have the same gate on 18009 and 18082.
//
// It scans this package's constants for any name that says "inbound" and a
// transport ("udp", "quic", "h3", "http3") and "port", and requires its value to
// equal defaultInboundPort. Today no such constant exists -- the QUIC inbound
// uses defaultInboundPort directly -- and that is the point: green now, red on
// the exact commit that would allocate `inboundQUICPort = 18010`.
func TestInboundPortIsSharedAcrossTransports(t *testing.T) {
	inbound := regexp.MustCompile(`(?i)inbound`)
	transport := regexp.MustCompile(`(?i)(udp|quic|h3|http3)`)
	port := regexp.MustCompile(`(?i)port`)

	entries, err := proxySources.ReadDir(".")
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
		src, err := proxySources.ReadFile(f)
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
					if !(inbound.MatchString(n) && transport.MatchString(n) && port.MatchString(n)) {
						continue
					}
					found++
					require.Less(t, i, len(vs.Values), "%s: transport-specific inbound port constant with no literal value", n)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok, "%s: inbound port constants must be integer literals so this test can read them", n)
					v, err := strconv.Atoi(lit.Value)
					require.NoError(t, err, n)
					assert.Equal(t, defaultInboundPort, v,
						"%s = %d: the HTTP/3 inbound binds the SAME port number as the TCP inbound (%d); a second number is a second thing the source must know per destination (038 R3)",
						n, v, defaultInboundPort)
				}
			}
		}
	}

	// Anti-vacuity: the scan must have seen the constant it guards.
	src, err := proxySources.ReadFile("ingress.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "defaultInboundPort = "+strconv.Itoa(defaultInboundPort),
		"the constant this test guards must be defined in ingress.go with a literal value")
	t.Logf("transport-specific inbound port constants found: %d (0 is the expected steady state)", found)
}
