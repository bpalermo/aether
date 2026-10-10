package proxy

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The header name and its format are spelled out in this file, never taken
// from the production constants: they are a wire contract between agent
// versions (a caller of one release reads what a destination of another
// wrote), so a change to either must fail here and be made on purpose.
const (
	wireOutcomeHeader = "x-aether-outcome"
	wireOutcomeFormat = "%RESPONSE_CODE%;%RESPONSE_FLAGS%;%REQ(:METHOD)%;%RESPONSE_CODE_DETAILS%"
	wireRetryOn       = "connect-failure,refused-stream,reset-before-request,retriable-headers"
)

func TestOutcomeWireContract(t *testing.T) {
	assert.Equal(t, wireOutcomeHeader, OutcomeHeader)
	assert.Equal(t, wireOutcomeFormat, outcomeFormat)
}

// inboundHCMs returns every HTTP connection manager of an inbound listener,
// keyed by filter chain name.
func inboundHCMs(t *testing.T, l *listenerv3.Listener) map[string]*http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	out := map[string]*http_connection_managerv3.HttpConnectionManager{}
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			h := &http_connection_managerv3.HttpConnectionManager{}
			if f.GetTypedConfig().MessageIs(h) {
				require.NoError(t, f.GetTypedConfig().UnmarshalTo(h))
				out[fc.GetName()] = h
			}
		}
	}
	return out
}

// TestInboundStampsTheOutcomeOnEveryResponse: every inbound connection manager
// (mTLS TCP on each HTTP port, cleartext, HTTP/3) stamps the outcome header on
// its virtual host, overwriting what the application set, and does nothing
// else to a response: no local-reply rewriting, and no opinion about
// x-envoy-ratelimited (aether#1641).
func TestInboundStampsTheOutcomeOnEveryResponse(t *testing.T) {
	pod := quicTestPod() // HTTP ports 8080 and 8081, raw TCP 9000
	mtls, err := NewInboundListener(pod, "example.org", false, false, nil, nil)
	require.NoError(t, err)
	cleartext, err := NewInboundListener(pod, "example.org", false, true, nil, nil)
	require.NoError(t, err)
	quic, err := NewInboundQUICListener(pod, "example.org", "mesh.local", false, false, nil, nil)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		l        *listenerv3.Listener
		wantHCMs int
	}{
		"mtls":      {mtls, 3}, // the h2 chain and one per HTTP port
		"cleartext": {cleartext, 1},
		"quic":      {quic, 2}, // the primary port's default chain and 8081
	} {
		t.Run(name, func(t *testing.T) {
			hcms := inboundHCMs(t, tc.l)
			require.Lenf(t, hcms, tc.wantHCMs, "connection managers of %s", tc.l.GetName())
			for chain, h := range hcms {
				assert.Nilf(t, h.GetLocalReplyConfig(), "%s: the status and body of a local reply are Envoy's", chain)
				vhosts := h.GetRouteConfig().GetVirtualHosts()
				require.NotEmptyf(t, vhosts, chain)
				for _, vh := range vhosts {
					require.Lenf(t, vh.GetResponseHeadersToAdd(), 1, "%s/%s", chain, vh.GetName())
					add := vh.GetResponseHeadersToAdd()[0]
					assert.Equalf(t, wireOutcomeHeader, add.GetHeader().GetKey(), chain)
					assert.Equalf(t, wireOutcomeFormat, add.GetHeader().GetValue(), chain)
					assert.Equalf(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, add.GetAppendAction(),
						"%s: the application must not be able to write its own outcome", chain)
					assert.Emptyf(t, vh.GetResponseHeadersToRemove(), "%s: nothing is removed from the application's response", chain)
				}
			}
		})
	}
}

// envoyResponseFlags is every short response flag of the pinned Envoy
// (source/common/stream_info/utility.h, CORE_RESPONSE_FLAGS), so that the
// regex is checked against each one, known to the rule or not.
var envoyResponseFlags = []string{
	"LH", "UH", "UT", "LR", "UR", "UF", "UC", "UO", "NR", "DI", "FI", "RL", "UAEX", "RLSE", "DC", "URX", "SI", "IH",
	"DPE", "UPE", "UMSDR", "RFCF", "NFCF", "DT", "OM", "DF", "DO", "DR", "UDO", "NC",
}

// The test's own statement of the rule, written without a regex.
var (
	wantNeverBegun = []string{"UF", "UH", "UO", "NC"}
	wantBegun      = []string{"UC", "UR", "LR"}
	wantIdempotent = []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"}
)

// wantRetried is the rule for a LOCAL reply with the given flags.
func wantRetried(method string, flags []string, details string) bool {
	if len(flags) == 0 {
		return false
	}
	allNeverBegun, allKnown := true, true
	for _, f := range flags {
		never, begun := slices.Contains(wantNeverBegun, f), slices.Contains(wantBegun, f)
		allNeverBegun = allNeverBegun && never
		allKnown = allKnown && (never || begun)
	}
	switch {
	case allNeverBegun:
		return true
	case !allKnown:
		return false
	default: // every flag is known and at least one says the request had begun
		return slices.Contains(wantIdempotent, method) || strings.Contains(details, "remote_refused_stream_reset")
	}
}

// TestOutcomeRetriableRegex: the caller's whole rule, against values as the
// pinned proxy renders them (the live ones are in //agent/test/mtlspool
// TestBegunRequestIsNotReplayed) and against every flag Envoy has.
func TestOutcomeRetriableRegex(t *testing.T) {
	// Envoy matches the whole value (RE2 FullMatch); so does this test.
	require.False(t, strings.HasPrefix(outcomeRetriableRegex, "^") || strings.HasSuffix(outcomeRetriableRegex, "$"))
	re := regexp.MustCompile(`^(?:` + outcomeRetriableRegex + `)$`)

	const (
		refused  = "upstream_reset_before_response_started{remote_connection_failure|delayed_connect_error:_Connection_refused}"
		termed   = "upstream_reset_before_response_started{connection_termination}"
		reset    = "upstream_reset_before_response_started{remote_reset}"
		refusedS = "upstream_reset_before_response_started{remote_refused_stream_reset}"
		localRef = "upstream_reset_before_response_started{local_refused_stream_reset}"
	)
	for _, tc := range []struct {
		value string
		want  bool
		why   string
	}{
		// The application answered.
		{"200;-;GET;via_upstream", false, "a 200 is never retried"},
		{"200;-;POST;via_upstream", false, "a 200 is never retried"},
		{"200;UF;POST;via_upstream", false, "a 200 is never retried, whatever the flags"},
		{"200;UF;POST;" + refused, false, "a 200 is never retried, whatever the rest says"},
		{"200;UC;GET;" + termed, false, "a 200 is never retried"},
		{"200;UR;POST;" + refusedS, false, "a 200 is never retried"},
		{"503;-;GET;via_upstream", true, "the application asked for another endpoint"},
		{"503;-;POST;via_upstream", true, "the application asked for another endpoint"},
		{"503;-;PURGE;via_upstream", true, "any method"},
		{"500;-;GET;via_upstream", false, "an application error"},
		{"502;-;GET;via_upstream", false, "an application error"},
		{"504;-;GET;via_upstream", false, "an application error"},
		{"429;-;GET;via_upstream", false, "an application verdict"},
		{"404;-;GET;via_upstream", false, "an application answer"},
		{"5030;-;GET;via_upstream", false, "not a 503"},
		{"1503;-;GET;via_upstream", false, "not a 503"},
		{"503;UC;POST;" + termed, false, "a 503 with a flag is not an application's answer as this proxy writes it"},
		{"503;UF;POST;" + refused, false, "a local reply is code 0"},
		{"503;UC;GET;" + termed, false, "a local reply is code 0"},
		{"500;UF;GET;" + refused, false, "only a local reply (0)"},
		{"404;NC;GET;cluster_not_found", false, "only a local reply (0)"},
		{"00;UF;GET;" + refused, false, "not 0"},
		{"10;UF;GET;" + refused, false, "not 0"},

		// The destination never began.
		{"0;UF;GET;" + refused, true, "connect failure"},
		{"0;UF;POST;" + refused, true, "connect failure, any method"},
		{"0;UF;POST;upstream_reset_before_response_started{connection_timeout}", true, "connect timeout"},
		{"0;UH;POST;no_healthy_upstream", true, "no healthy upstream"},
		{"0;UO;POST;overflow", true, "circuit breaker"},
		{"0;NC;POST;cluster_not_found", true, "app cluster gone"},
		{"0;UF,UH;POST;whatever", true, "two never-begun flags"},
		{"0;UH,UF,NC;POST;whatever", true, "three never-begun flags, any order"},
		{"0;UF;POST;a;b;c", true, "details may contain the separator"},
		{"0;UF;POST;", true, "empty details"},
		{"0;UF;POST;café", false, "details are ASCII; anything else fails closed"},
		{"0;UF;M-SEARCH;x", true, "any method token"},
		{"0;LH;POST;health_check_failed", false, "not a flag the inbound route produces"},

		// The destination had begun.
		{"0;UC;POST;" + termed, false, "the application may have run it"},
		{"0;UC;PATCH;" + termed, false, "the application may have run it"},
		{"0;UR;POST;" + reset, false, "the application may have run it"},
		{"0;LR;POST;upstream_reset_before_response_started{local_reset}", false, "the application may have run it"},
		{"0;UF,UC;POST;" + termed, false, "one begun flag is enough"},
		{"0;UC,UF;POST;" + termed, false, "one begun flag is enough, any order"},
		{"0;UC;GET;" + termed, true, "idempotent"},
		{"0;UR;HEAD;" + reset, true, "idempotent"},
		{"0;LR;OPTIONS;x", true, "idempotent"},
		{"0;UC;TRACE;x", true, "idempotent"},
		{"0;UC;PUT;" + termed, true, "idempotent"},
		{"0;UC,UF;DELETE;" + termed, true, "idempotent, two known flags"},
		{"0;UC;get;" + termed, false, "methods are case-sensitive; an unknown one is not idempotent"},
		{"0;UC;GETX;" + termed, false, "not GET"},
		{"0;UC;XGET;" + termed, false, "not GET"},
		{"0;UC;CONNECT;" + termed, false, "not idempotent"},
		{"0;UC;POST;GET;" + termed, false, "the method is the third field, not something in the details"},
		{"0;UC;P;GET;" + termed, false, "a method cannot contain the separator"},

		// The application refused the stream: not processed, by the protocol.
		{"0;UR;POST;" + refusedS, true, "REFUSED_STREAM, any method"},
		{"0;UR;PATCH;" + refusedS, true, "REFUSED_STREAM, any method"},
		{"0;UR;GET;" + refusedS, true, "REFUSED_STREAM"},
		{"0;UR,UF;POST;" + refusedS, true, "with a never-begun flag"},
		{"0;UR,UT;POST;" + refusedS, false, "an unknown companion flag fails closed"},
		{"200;UR;POST;" + refusedS, false, "never a 200"},
		{"0;LR;POST;" + localRef, false, "only the application's refusal: local_refused_stream_reset is another reason"},
		{"0;UC;POST;x{connection_termination|Connection_refused}", false, "a 'refused' that is not a refused stream"},
		{"0;UC;e_refused;" + termed, false, "a method spelled like the detail is still a method"},

		// Begun, and never replayed whatever the method.
		{"0;UT;GET;response_timeout", false, "a timeout: the application may still be running it"},
		{"0;UPE;GET;upstream_reset_before_response_started{protocol_error}", false, "a malformed answer"},
		{"0;UC,UT;GET;x", false, "an unknown companion flag fails closed"},
		{"0;UF,URX;GET;x", false, "an unknown companion flag fails closed"},
		{"0;NEWFLAG;GET;x", false, "a flag a future Envoy adds fails closed"},
		{"0;UFX;GET;x", false, "not UF"},
		{"0;XUF;GET;x", false, "not UF"},
		{"0;UF UH;GET;x", false, "malformed set"},
		{"0;,UF;GET;x", false, "malformed set"},
		{"0;UF,,UH;GET;x", false, "malformed set"},

		// Local replies that are not the router's.
		{"0;-;GET;direct_response", false, "no flag, no rule"},
		{"0;NR;GET;route_not_found", false, "a 404"},
		{"0;UAEX;GET;ext_authz_denied", false, "a verdict"},
		{"0;RL;GET;request_rate_limited", false, "a verdict"},
		{"0;OM;GET;overload", false, "may have begun"},

		// Not the grammar at all.
		{"", false, "empty"},
		{"-", false, "garbage"},
		{"503", false, "garbage"},
		{"0", false, "garbage"},
		{"0;UF", false, "two fields"},
		{"0;;GET;x", false, "no flags"},
		{";UF;GET;x", false, "no code"},
		{"0 UF GET x", false, "wrong separator"},
		{"x;0;UF;GET;x", false, "a field too many in front"},
		{"GET;0;UF;x", false, "the grammar of an earlier draft: method first"},
		{"0;UF;POST;x\n503;-;GET;via_upstream", false, "one value, one line"},
	} {
		assert.Equalf(t, tc.want, re.MatchString(tc.value), "%q: %s", tc.value, tc.why)
	}

	// What the compact set spelling additionally accepts. No Envoy writes
	// these (the formatter joins flags with ","); they are here so that the
	// looseness is a known quantity and stays inside the flag field.
	for _, value := range []string{"0;UFUH;POST;x", "0;UF,;POST;x"} {
		assert.Truef(t, re.MatchString(value), "%q", value)
	}

	// Every flag Envoy has, alone and paired with every other, as a local
	// reply for an idempotent and a non-idempotent method, with and without
	// the refused-stream detail.
	checked := 0
	for _, method := range []string{"GET", "POST"} {
		for _, details := range []string{termed, refusedS} {
			for _, a := range envoyResponseFlags {
				sets := [][]string{{a}}
				for _, b := range envoyResponseFlags {
					if a != b {
						sets = append(sets, []string{a, b})
					}
				}
				for _, set := range sets {
					value := fmt.Sprintf("0;%s;%s;%s", strings.Join(set, ","), method, details)
					if got, want := re.MatchString(value), wantRetried(method, set, details); got != want {
						t.Errorf("%q: retried=%v, want %v", value, got, want)
					}
					checked++
					// The same flags on any real status are never retried.
					for _, code := range []string{"200", "500", "404", "504", "503"} {
						value := fmt.Sprintf("%s;%s;%s;%s", code, strings.Join(set, ","), method, details)
						assert.Falsef(t, re.MatchString(value), "%q", value)
					}
				}
			}
		}
	}
	assert.Equal(t, 2*2*len(envoyResponseFlags)*len(envoyResponseFlags), checked)

	// The production lists are the test's, and the hand-spelled regex agrees
	// with them (the sweep above is what proves it for the flags).
	assert.Equal(t, wantNeverBegun, outcomeNeverBegunFlags)
	assert.Equal(t, wantBegun, outcomeBegunFlags)
	assert.Equal(t, wantIdempotent, outcomeIdempotentMethods)
	assert.Contains(t, outcomeRetriableRegex, "(?:"+strings.Join(outcomeIdempotentMethods, "|")+");")
	for _, f := range append(slices.Clone(wantNeverBegun), wantBegun...) {
		assert.Containsf(t, envoyResponseFlags, f, "%s is not a response flag Envoy has", f)
	}
	assert.Contains(t, refusedS, outcomeRefusedStreamDetail)
	for _, other := range []string{refused, termed, reset, localRef} {
		assert.NotContainsf(t, other, outcomeRefusedStreamDetail, "the refused-stream detail must not be in %q", other)
	}
}

// requireMeshRetryPolicy fails unless rp is the header-driven policy and
// nothing more.
func requireMeshRetryPolicy(t *testing.T, where string, rp *routev3.RetryPolicy) {
	t.Helper()
	require.NotNilf(t, rp, where)
	assert.Equalf(t, wireRetryOn, rp.GetRetryOn(), where)
	assert.Emptyf(t, rp.GetRetriableStatusCodes(), "%s: a status-code condition would replay a begun POST again", where)
	assert.Emptyf(t, rp.GetRetriableRequestHeaders(), where)
	require.Lenf(t, rp.GetRetriableHeaders(), 1, "%s: ONE matcher: retriable headers are OR-ed", where)
	m := rp.GetRetriableHeaders()[0]
	assert.Equalf(t, wireOutcomeHeader, m.GetName(), where)
	assert.Equalf(t, outcomeRetriableRegex, m.GetStringMatch().GetSafeRegex().GetRegex(), where)
	assert.Falsef(t, m.GetInvertMatch(), where)
	assert.Falsef(t, m.GetTreatMissingHeaderAsEmpty(), "%s: a response without the header must not match", where)
	assert.Equalf(t, uint32(2), rp.GetNumRetries().GetValue(), where)
	require.Lenf(t, rp.GetRetryHostPredicate(), 1, where)
	assert.Equalf(t, "envoy.retry_host_predicates.previous_hosts", rp.GetRetryHostPredicate()[0].GetName(), where)
}

// TestOutboundRetryPolicyReadsOnlyTheOutcome guards the conditions that would
// undo aether#1641 if they came back: retry conditions are OR-ed, so any
// status-code or reset condition retries the 503 of a begun POST whatever its
// outcome header says.
func TestOutboundRetryPolicyReadsOnlyTheOutcome(t *testing.T) {
	rp := outboundRetryPolicy()
	requireMeshRetryPolicy(t, "outboundRetryPolicy", rp)
	conditions := strings.Split(rp.GetRetryOn(), ",")
	for _, c := range []string{
		"retriable-status-codes", "5xx", "gateway-error", "reset", "retriable-4xx", "envoy-ratelimited",
		"unavailable", "internal", "cancelled", "deadline-exceeded", "resource-exhausted", "http3-post-connect-failure",
	} {
		assert.NotContainsf(t, conditions, c, "retry_on must never contain %s", c)
	}
}

// retryRoutes walks a route table and returns its routes that carry a retry
// policy, by a readable name.
func retryRoutes(vhosts []*routev3.VirtualHost) map[string]*routev3.Route {
	out := map[string]*routev3.Route{}
	for _, vh := range vhosts {
		for i, r := range vh.GetRoutes() {
			if r.GetRoute().GetRetryPolicy() != nil {
				out[fmt.Sprintf("%s#%d", vh.GetName(), i)] = r
			}
		}
	}
	return out
}

// TestCallerRoutesMatchAndRemoveTheOutcome: every caller-side route that
// retries does so on the outcome header only, and removes that header from the
// response it forwards, so it reaches no client application; a route to
// something outside the mesh does not touch it; and the operator's own
// response-header removals survive.
func TestCallerRoutesMatchAndRemoveTheOutcome(t *testing.T) {
	gamma := BuildOutboundServiceVirtualHost("web.shop.mesh.local", []string{"web.shop.mesh.local"}, []GammaRoute{
		{
			Matches:        []GammaMatch{{Prefix: "/a"}, {Prefix: "/b"}},
			Backends:       []GammaBackend{{Cluster: "v1.shop.mesh.local", Weight: 1}, {Cluster: "v2.shop.mesh.local", Weight: 1}},
			HeaderMutation: &GammaHeaderMutation{RemoveResponse: []string{"x-internal"}},
		},
		{Matches: []GammaMatch{{Prefix: "/plain"}}},
		{Matches: []GammaMatch{{Prefix: "/moved"}}, Redirect: &GammaRedirect{Hostname: "elsewhere.example"}},
	})
	outbound := BuildOutboundRouteConfiguration([]*routev3.VirtualHost{
		BuildOutboundClusterVirtualHost("api.shop.mesh.local", []string{"api.shop.mesh.local"}),
		gamma,
	}, "mesh.local")
	capture := BuildCaptureRouteConfiguration(nil, "mesh.local", true,
		KnownTargetRoute{AuthorityRegex: `^api(\.shop)?(:[0-9]+)?$`, Cluster: "api.shop.mesh.local"})

	for name, rc := range map[string]*routev3.RouteConfiguration{"out_http": outbound, "cap_http": capture} {
		routes := retryRoutes(rc.GetVirtualHosts())
		require.NotEmptyf(t, routes, "%s has no retrying route", name)
		for rn, r := range routes {
			requireMeshRetryPolicy(t, name+" "+rn, r.GetRoute().GetRetryPolicy())
			assert.Equalf(t, 1, countOf(r.GetResponseHeadersToRemove(), wireOutcomeHeader), "%s %s removes the outcome header once", name, rn)
		}
		// Nothing above the route removes it: the capture table's passthrough
		// route reaches destinations outside the mesh, whose headers are theirs.
		assert.NotContainsf(t, rc.GetResponseHeadersToRemove(), wireOutcomeHeader, "%s: not at the route configuration", name)
		for _, vh := range rc.GetVirtualHosts() {
			assert.NotContainsf(t, vh.GetResponseHeadersToRemove(), wireOutcomeHeader, "%s/%s: not at the virtual host", name, vh.GetName())
			for _, r := range vh.GetRoutes() {
				if r.GetRoute().GetRetryPolicy() == nil {
					assert.NotContainsf(t, r.GetResponseHeadersToRemove(), wireOutcomeHeader,
						"%s/%s: a route that does not go to a mesh service (direct response, redirect, passthrough) must leave the header alone", name, vh.GetName())
				}
			}
		}
	}
	// The retry-carrying routes this table is known to have: the on-demand
	// route plus the known target (cap_http), and on out_http the cluster
	// vhost, three GAMMA matches, the GAMMA catch-all and the on-demand route.
	assert.Len(t, retryRoutes(capture.GetVirtualHosts()), 2)
	assert.Len(t, retryRoutes(outbound.GetVirtualHosts()), 6)

	// A rule's own removals are kept, and its matches do not share one slice.
	var a, b *routev3.Route
	for _, r := range gamma.GetRoutes() {
		switch r.GetMatch().GetPathSeparatedPrefix() {
		case "/a":
			a = r
		case "/b":
			b = r
		}
	}
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.Equal(t, []string{"x-internal", wireOutcomeHeader}, a.GetResponseHeadersToRemove())
	assert.Equal(t, []string{"x-internal", wireOutcomeHeader}, b.GetResponseHeadersToRemove())
	a.ResponseHeadersToRemove[0] = "changed"
	assert.Equal(t, "x-internal", b.GetResponseHeadersToRemove()[0], "the two matches of one rule must not share a slice")
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// TestEdgeRoutesByBackendKind: at the edge a route to a mesh service reads the
// outcome header and removes it before the external client; a route whose
// backends are all cleartext Kubernetes Services has no destination proxy to
// stamp one, keeps the status-code policy and leaves the backend's headers
// alone; a rule that splits between the two takes the mesh policy.
func TestEdgeRoutesByBackendKind(t *testing.T) {
	mesh := "web.shop.mesh.local"
	k8s := EdgeK8sClusterName("shop", "legacy", 8080)
	requireNonMesh := func(where string, rp *routev3.RetryPolicy) {
		t.Helper()
		require.NotNilf(t, rp, where)
		assert.Equalf(t, "connect-failure,refused-stream,reset-before-request,retriable-status-codes", rp.GetRetryOn(), where)
		assert.Equalf(t, []uint32{503}, rp.GetRetriableStatusCodes(), where)
		assert.Emptyf(t, rp.GetRetriableHeaders(), where)
		assert.Equalf(t, uint32(2), rp.GetNumRetries().GetValue(), where)
		require.Lenf(t, rp.GetRetryHostPredicate(), 1, where)
	}

	r := BuildEdgeRoute("/", "", nil, "", nil, mesh, &GammaHeaderMutation{RemoveResponse: []string{"x-internal"}}, nil, nil, nil)
	requireMeshRetryPolicy(t, "edge, mesh backend", r.GetRoute().GetRetryPolicy())
	assert.Equal(t, []string{"x-internal", wireOutcomeHeader}, r.GetResponseHeadersToRemove())

	r = BuildEdgeRoute("/", "", nil, "", nil, k8s, nil, nil, nil, nil)
	requireNonMesh("edge, cleartext backend", r.GetRoute().GetRetryPolicy())
	assert.NotContains(t, r.GetResponseHeadersToRemove(), wireOutcomeHeader, "a cleartext Kubernetes Service is not behind a destination proxy")

	r = BuildEdgeRoute("/", "", nil, "", nil, "", nil, &GammaRedirect{Hostname: "elsewhere.example"}, nil, nil)
	assert.Nil(t, r.GetRoute())
	assert.NotContains(t, r.GetResponseHeadersToRemove(), wireOutcomeHeader, "a redirect has no upstream response")

	r = BuildEdgeRouteWeighted("/", "", nil, "", nil, []WeightedRouteBackend{{Cluster: mesh, Weight: 1}, {Cluster: k8s, Weight: 1}}, nil, nil, nil, nil)
	requireMeshRetryPolicy(t, "edge, split rule", r.GetRoute().GetRetryPolicy())
	assert.NotContains(t, r.GetResponseHeadersToRemove(), wireOutcomeHeader, "a split rule removes per backend")
	byName := map[string][]string{}
	for _, cw := range r.GetRoute().GetWeightedClusters().GetClusters() {
		byName[cw.GetName()] = cw.GetResponseHeadersToRemove()
	}
	assert.Equal(t, []string{wireOutcomeHeader}, byName[mesh])
	assert.Empty(t, byName[k8s])

	r = BuildEdgeRouteWeighted("/", "", nil, "", nil, []WeightedRouteBackend{{Cluster: k8s, Weight: 1}, {Cluster: EdgeK8sClusterName("shop", "other", 80), Weight: 1}}, nil, nil, nil, nil)
	requireNonMesh("edge, split between two cleartext backends", r.GetRoute().GetRetryPolicy())

	// One backend through the weighted builder is the plain route.
	r = BuildEdgeRouteWeighted("/", "", nil, "", nil, []WeightedRouteBackend{{Cluster: mesh, Weight: 1}}, nil, nil, nil, nil)
	requireMeshRetryPolicy(t, "edge, one mesh backend", r.GetRoute().GetRetryPolicy())
	assert.Contains(t, r.GetResponseHeadersToRemove(), wireOutcomeHeader)
}
