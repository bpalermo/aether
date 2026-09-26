package proxy

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

// proxySources is this package's own non-test source, embedded so the scan
// works inside Bazel's sandbox where the cwd holds no sources.
//
//go:embed *.go
var proxySources embed.FS

// TestEastWestPortIsSharedAcrossTransports pins the rule on
// DefaultEastWestTunnelPort: east-west QUIC binds UDP on the SAME number as the
// TCP tunnel, never a new port. The edge already works this way
// (BuildEdgeGatewayHTTP3Listener shares internalPort with the TCP HTTPS
// listener), and TCP and QUIC on different capture mechanisms is the split that
// produced #916.
//
// It scans this package's source for any exported or unexported constant whose
// name says "east-west" AND "quic"/"h3"/"udp" AND "port", and requires its value
// to equal DefaultEastWestTunnelPort. Today no such constant exists, and that is
// the point: the test is green now and turns red on the exact commit that would
// violate the rule, with the offending name in the failure. A written constraint
// with no gate is a wish (#853).
func TestEastWestPortIsSharedAcrossTransports(t *testing.T) {
	eastWest := regexp.MustCompile(`(?i)east.?west`)
	transport := regexp.MustCompile(`(?i)(quic|h3|http3|udp)`)
	port := regexp.MustCompile(`(?i)port`)

	// Under Bazel the test's cwd carries no package sources, so they are
	// embedded (the same mechanism as cachemetrics' seed-policy test). A glob
	// rather than a named file, so a constant added in a NEW file is still seen.
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
					if !(eastWest.MatchString(n) && transport.MatchString(n) && port.MatchString(n)) {
						continue
					}
					found++
					require.Less(t, i, len(vs.Values), "%s: east-west transport port constant with no literal value", n)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok, "%s: east-west port constants must be integer literals so this test can read them", n)
					v, err := strconv.Atoi(lit.Value)
					require.NoError(t, err, n)
					assert.Equal(t, DefaultEastWestTunnelPort, v,
						"%s = %d: east-west QUIC/UDP must bind the SAME port as the TCP tunnel (%d), like the edge's H3 listener; a second number splits capture (#916)",
						n, v, DefaultEastWestTunnelPort)
				}
			}
		}
	}

	// Anti-vacuity for the OTHER direction: the scan must at least have seen the
	// TCP tunnel constant itself, or the parse is not reading this package.
	src, err := proxySources.ReadFile("edge.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "DefaultEastWestTunnelPort = "+strconv.Itoa(DefaultEastWestTunnelPort),
		"the constant this test guards must be defined in edge.go with a literal value")
	t.Logf("east-west transport-specific port constants found: %d (0 is the expected steady state)", found)
}
