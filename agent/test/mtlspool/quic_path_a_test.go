package mtlspool

// aether#957 path A, validated against the real proxy binary.
//
// Envoy's QUIC client verified the server leaf's DNS SANs against the SNI
// AFTER any configured validator had succeeded
// (source/common/quic/envoy_quic_proof_verifier.cc,
// verifyLeafCertMatchesHostname), so a URI-SAN-only X.509-SVID behind a
// port-shaped SNI ("18008") could never complete a handshake. The shipped
// workaround (path B, quic_selection_test.go) gives the destination DNS SANs
// and dials "<port>.<fqdn>". The upstream fix, envoyproxy/envoy#47740 (carried
// in the proxy build by aether#972), skips that hostname check when an
// explicit identity check -- match_typed_subject_alt_names, pins, a custom
// validator -- is configured and auto_sni_san_validation is off, behind the
// runtime guard quicHostnameGuard (default true).
//
// These tests run the ORIGINAL path-A configuration: the destination presents
// the URI-only SVID shape (pki.leaf) and the twins dial the bare port SNI.
//
// Expected state per binary (//agent/test/envoybin:envoy_bin is the image the chart
// pins):
//
//	                                         pin without #47740   pin with #47740
//	TestQUICPathAHandshakesWithoutDNSSANs    FAIL (hostname)      PASS
//	TestQUICPathAWrongSANStillFails          PASS                 PASS
//	TestQUICPathAGuardOffRestoresHostname... PASS                 PASS
//
// The first is RED until the pin moves; that is its point. The second must
// never turn red: the fix may not accept a peer the SAN matcher rejects. The
// third is the anti-vacuity control: it proves the harness can see the
// hostname failure at all, so the first passing means the check was deferred
// and not that the log plumbing lost the message. (On a binary without #47740
// the guard is not registered and the static layer is inert; the hostname
// check fails there because it is unconditional.)

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// quicHostnameGuard is the runtime guard envoyproxy/envoy#47740 adds
	// (source/common/runtime/runtime_features.cc, default true).
	quicHostnameGuard = "envoy.reloadable_features.quic_hostname_check_deferred_to_explicit_san_match"

	// hostnameMismatch is verifyLeafCertMatchesHostname's error detail at the
	// pin (envoy_quic_proof_verifier.cc): "Leaf certificate doesn't match
	// hostname: <sni>".
	hostnameMismatch = "doesn't match hostname"

	// sanMatcherMismatch is DefaultCertValidator::verifyCertAndUpdateStatus's
	// error detail when match_typed_subject_alt_names rejects the leaf
	// (source/common/tls/cert_validator/default_validator.cc at 726d7acb73:
	// "verify cert failed: SAN matcher, certificate SANs are [...]").
	sanMatcherMismatch = "verify cert failed: SAN matcher"

	// quicCertUnknown is the QUIC error code a failed server-cert verification
	// closes the connection with.
	quicCertUnknown = "QUIC_TLS_CERTIFICATE_UNKNOWN"

	// spiffeNotDest is a valid identity in the trust domain that is NOT the
	// destination: the negative control's SAN pin.
	spiffeNotDest = "spiffe://" + trustDomain + "/ns/demo/sa/not-echo"
)

// quicPathALogArgs raises exactly the two loggers that print the QUIC
// handshake failure; the rest of Envoy stays at warn. Observed at the pin:
//
//   - [info][quic] quiche's tls_handshaker.cc: "Cert chain verification
//     failed: <details>", where <details> is either verifyLeafCertMatchesHostname's
//     "Leaf certificate doesn't match hostname: 18008" or the default
//     validator's "verify cert failed: SAN matcher, certificate SANs are [...]".
//     Both markers therefore need only quic:info.
//   - [debug][pool] conn_pool_base.cc: "client disconnected, failure reason:
//     QUIC_TLS_CERTIFICATE_UNKNOWN with details: ..." (needs pool:debug).
//
// The 503 body carries neither ("reset reason: local connection failure"),
// so the log is the only place the cause is visible.
var quicPathALogArgs = []string{"--component-log-level", "quic:info,pool:debug"}

// quicOptions parameterises the HTTP/3 arm. defaultQUICOptions is path B,
// byte-for-byte what the two selection tests have always run.
type quicOptions struct {
	// uriOnlyLeaf gives the destination the X.509-SVID shape (URI SAN + the
	// harness's 127.0.0.1 IP SAN, no DNS SAN) instead of path B's DNS SANs.
	uriOnlyLeaf bool
	// sni is the twins' UpstreamTlsContext.sni.
	sni string
	// sanPin is the server identity the twins pin
	// (match_typed_subject_alt_names, URI).
	sanPin []string
	// guardOff sets quicHostnameGuard to false in a static runtime layer.
	guardOff bool
	// extraArgs are appended to Envoy's command line.
	extraArgs []string
	// log, when set, receives a copy of Envoy's stdout and stderr.
	log *syncBuffer
}

func defaultQUICOptions() quicOptions {
	return quicOptions{sni: quicSNI, sanPin: []string{spiffeDest}}
}

// pathAOptions is the original path-A configuration: URI-only leaf, bare port
// SNI, the correct pin, the guard at its default, logs captured.
func pathAOptions() quicOptions {
	return quicOptions{
		uriOnlyLeaf: true,
		sni:         upstreamSNI,
		sanPin:      []string{spiffeDest},
		extraArgs:   quicPathALogArgs,
		log:         &syncBuffer{},
	}
}

// layeredRuntime is nil unless the guard is forced off, so the default
// bootstrap is unchanged.
func (o quicOptions) layeredRuntime() *bootstrapv3.LayeredRuntime {
	if !o.guardOff {
		return nil
	}
	layer, err := structpb.NewStruct(map[string]any{quicHostnameGuard: false})
	if err != nil {
		panic(err)
	}
	return &bootstrapv3.LayeredRuntime{Layers: []*bootstrapv3.RuntimeLayer{{
		Name:           "static",
		LayerSpecifier: &bootstrapv3.RuntimeLayer_StaticLayer{StaticLayer: layer},
	}}}
}

// output is where Envoy's stdout/stderr go: the test log always, plus the
// capture buffer when one is set.
func (o quicOptions) output(t *testing.T) io.Writer {
	tw := &testWriter{t: t, prefix: "envoy"}
	if o.log == nil {
		return tw
	}
	return io.MultiWriter(tw, o.log)
}

// syncBuffer is a bytes.Buffer safe for exec's two copying goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logLines returns the captured lines containing needle, capped, for failure
// messages.
func (b *syncBuffer) logLines(needle string, limit int) []string {
	var out []string
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, needle) {
			out = append(out, l)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// attempt is one request's outcome, success or not.
type attempt struct {
	status int
	body   string
	san    string
	proto  string
	err    error
}

func (a attempt) String() string {
	if a.err != nil {
		return "err=" + a.err.Error()
	}
	return fmt.Sprintf("status=%d san=%q proto=%q body=%q", a.status, a.san, a.proto, a.body)
}

func (c *sourceClient) callOnce(t *testing.T) attempt {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base, nil)
	require.NoError(t, err)
	req.Host = "echo.demo.svc"
	resp, err := c.http.Do(req)
	if err != nil {
		return attempt{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return attempt{
		status: resp.StatusCode,
		body:   string(body),
		san:    resp.Header.Get("x-aether-peer-uri-san"),
		proto:  resp.Header.Get("x-aether-proto"),
	}
}

// startPathA runs both destinations and the proxy with o.
func startPathA(t *testing.T, o quicOptions) (a, b *sourceClient) {
	t.Helper()
	p := newPKI(t)
	h2 := startDestination(t, p)
	h3 := startDestinationH3With(t, p, o)
	h := startEnvoyQUICWith(t, p, h2.addr, h3.addr, selectionArms(), o)
	return newSourceClient("source-a", h.addrA), newSourceClient("source-b", h.addrB)
}

// untilLogOrDeadline sends requests from both sources until the log contains
// marker or the deadline passes, returning every attempt made. Negative tests
// use it so a slow first handshake (SDS warm-up) is not mistaken for the
// failure under test, and the failure under test is not missed.
func untilLogOrDeadline(t *testing.T, log *syncBuffer, marker string, clients ...*sourceClient) map[string][]attempt {
	t.Helper()
	got := map[string][]attempt{}
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, c := range clients {
			got[c.name] = append(got[c.name], c.callOnce(t))
		}
		if strings.Contains(log.String(), marker) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A few more once the marker is in: the failure must be steady, not a
	// warm-up blip that a retry would have hidden.
	for range 3 {
		for _, c := range clients {
			got[c.name] = append(got[c.name], c.callOnce(t))
		}
	}
	return got
}

func assertNoneOK(t *testing.T, got map[string][]attempt) {
	t.Helper()
	for name, as := range got {
		for i, a := range as {
			assert.NotEqualf(t, http.StatusOK, a.status, "%s attempt %d succeeded (%s): the handshake must fail", name, i, a)
		}
		if len(as) > 0 {
			t.Logf("%s: %d attempts, last: %s", name, len(as), as[len(as)-1])
		}
	}
}

// TestQUICPathAHandshakesWithoutDNSSANs: URI-only destination SVID, bare port
// SNI, correct SAN pin, guard at its default. With #47740 in the binary the
// SAN matcher is the identity check and the hostname check is skipped, so
// every request answers 200 over HTTP/3 carrying the caller's own identity.
//
// RED on a binary without #47740: every request 503s and the log shows
// "Leaf certificate doesn't match hostname: 18008".
func TestQUICPathAHandshakesWithoutDNSSANs(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	o := pathAOptions()
	a, b := startPathA(t, o)
	defer func() {
		if t.Failed() {
			t.Logf("hostname lines: %q", o.log.logLines(hostnameMismatch, 5))
			t.Logf("%s lines: %q", quicCertUnknown, o.log.logLines(quicCertUnknown, 5))
		}
	}()

	for _, c := range []*sourceClient{a, b} {
		// Warm-up: the first request can race the SDS fetch.
		deadline := time.Now().Add(15 * time.Second)
		var last attempt
		for {
			last = c.callOnce(t)
			if last.status == http.StatusOK || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		require.Equalf(t, http.StatusOK, last.status, "%s: no 200 before the deadline over a URI-only SVID with SNI %q (last: %s)", c.name, upstreamSNI, last)
	}
	want := map[*sourceClient]string{a: spiffeSourceA, b: spiffeSourceB}
	for i := 0; i < exchangeRounds; i++ {
		for _, c := range []*sourceClient{a, b} {
			got := c.callOnce(t)
			assert.Equalf(t, http.StatusOK, got.status, "%s request %d: %s", c.name, i, got)
			assert.Equalf(t, "HTTP/3.0", got.proto, "%s request %d did not ride HTTP/3", c.name, i)
			assert.Equalf(t, want[c], got.san, "%s request %d was verified as the wrong identity", c.name, i)
		}
	}
	log := o.log.String()
	assert.NotContains(t, log, hostnameMismatch, "the QUIC hostname check still ran over an explicit SAN matcher")
	assert.NotContains(t, log, quicCertUnknown, "a QUIC handshake failed on the server certificate")
}

// TestQUICPathAWrongSANStillFails is the negative control: the same shape with
// the twins pinning an identity the destination does not have. The fix must
// not accept anything the SAN matcher rejects, so the handshake fails -- and
// with #47740 it fails ON the SAN matcher, not on the hostname.
//
// This one is GREEN on both binaries, observed at the pin without #47740:
// VerifyCertChain runs the configured validator FIRST and only reaches the
// hostname check after it succeeds, so a wrong pin already fails on the SAN
// matcher today and "doesn't match hostname" never appears. Its job is to stay
// green once the fix lands -- deferring the hostname check must never turn a
// SAN-matcher rejection into an accepted peer.
func TestQUICPathAWrongSANStillFails(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	o := pathAOptions()
	o.sanPin = []string{spiffeNotDest}
	a, b := startPathA(t, o)
	got := untilLogOrDeadline(t, o.log, sanMatcherMismatch, a, b)
	assertNoneOK(t, got)
	assert.Contains(t, o.log.String(), sanMatcherMismatch, "the handshake must fail on the SAN matcher")
	assert.NotContains(t, o.log.String(), hostnameMismatch, "the hostname check must not be what rejects the peer")
}

// TestQUICPathAGuardOffRestoresHostnameCheck is the guard control and the
// anti-vacuity check: correct pin, guard forced false -> the hostname check
// runs and the handshake fails with the hostname message. Passes on both
// binaries (unconditional check before #47740; guard off after it).
func TestQUICPathAGuardOffRestoresHostnameCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	o := pathAOptions()
	o.guardOff = true
	a, b := startPathA(t, o)
	got := untilLogOrDeadline(t, o.log, hostnameMismatch, a, b)
	assertNoneOK(t, got)
	assert.Contains(t, o.log.String(), hostnameMismatch, "with the guard off the QUIC hostname check must reject a URI-only SVID behind SNI %q", upstreamSNI)
	// Also proves quicPathALogArgs surfaces the pool's failure code, so the
	// positive test's NotContains(quicCertUnknown) is not vacuous.
	assert.Contains(t, o.log.String(), quicCertUnknown, "the pool's QUIC failure reason is not in the captured log")
}

// TestQUICTwinsLeaveAutoSNIOff: #47740 deliberately KEEPS the hostname check
// when auto_sni_san_validation is on, so path A only holds while no twin sets
// it (or the auto_sni family on the protocol options). Checked on every twin
// shape the harness builds -- production's QUICClusterFrom output.
func TestQUICTwinsLeaveAutoSNIOff(t *testing.T) {
	shapes := map[string]quicOptions{"path-b": defaultQUICOptions(), "path-a": pathAOptions()}
	for name, o := range shapes {
		for _, src := range []string{spiffeSourceA, spiffeSourceB} {
			cl := quicTwinWith(t, quicTwinA, src, "127.0.0.1:1", o)
			var q quicv3.QuicUpstreamTransport
			require.NoError(t, cl.GetTransportSocket().GetTypedConfig().UnmarshalTo(&q))
			tlsCtx := q.GetUpstreamTlsContext()
			assert.Falsef(t, tlsCtx.GetAutoHostSni(), "%s/%s: auto_host_sni set", name, src)
			assert.Falsef(t, tlsCtx.GetAutoSniSanValidation(), "%s/%s: auto_sni_san_validation set", name, src)
			assert.Equalf(t, o.sni, tlsCtx.GetSni(), "%s/%s: sni", name, src)
			// The explicit identity check path A defers to must be there.
			matchers := tlsCtx.GetCommonTlsContext().GetCombinedValidationContext().GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
			require.Lenf(t, matchers, len(o.sanPin), "%s/%s: match_typed_subject_alt_names", name, src)
			for i, m := range matchers {
				assert.Equalf(t, o.sanPin[i], m.GetMatcher().GetExact(), "%s/%s: SAN pin %d", name, src, i)
			}
			assertNoAutoSNIProtocolOptions(t, name+"/"+src, cl)
		}
	}
}

func assertNoAutoSNIProtocolOptions(t *testing.T, label string, cl *clusterv3.Cluster) {
	t.Helper()
	anyOpts, ok := cl.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
	require.Truef(t, ok, "%s: no HttpProtocolOptions", label)
	var opts httpv3.HttpProtocolOptions
	require.NoError(t, anyOpts.UnmarshalTo(&opts))
	up := opts.GetUpstreamHttpProtocolOptions()
	assert.Falsef(t, up.GetAutoSni(), "%s: upstream_http_protocol_options.auto_sni set", label)
	assert.Falsef(t, up.GetAutoSanValidation(), "%s: upstream_http_protocol_options.auto_san_validation set", label)
}
