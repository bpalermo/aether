package proxy

import (
	"slices"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
)

// The outcome header (aether#1641, aether#1646).
//
// A destination proxy answers 503 in two very different situations:
//
//   - it never reached its application: the connect was refused or timed out,
//     no host was healthy, the pool overflowed, the app cluster was not there.
//     Replaying the request on another endpoint is safe whatever its method,
//     and is what keeps a pod roll hitless;
//   - it had begun sending the request to the application, and the
//     application's connection then closed or reset before any response. The
//     application may have run the request. Replaying a POST runs it twice.
//
// The status code is 503 in both, so a caller that retries "a 503" replays the
// second kind. For a gRPC request it is wrong the other way round: both kinds
// come back as HTTP 200 with grpc-status 14, and a caller that retries "a 503"
// retries neither.
//
// So the destination says what happened, on EVERY response, in one response
// header, and the caller's retry policy reads that header and nothing else
// about the response (outboundRetryPolicy).
//
// # Grammar
//
// OutcomeHeader's value is four fields separated by ";":
//
//	<code> ";" <flags> ";" <method> ";" <details>
//
//	code     the HTTP status of a response the APPLICATION sent (200, 503, …),
//	         or 0 for a reply this proxy generated itself. That is what
//	         Envoy's %RESPONSE_CODE% holds when response headers are
//	         finalised: a local reply's code is recorded after that point.
//	         For a gRPC request the local reply goes out as HTTP 200 with a
//	         grpc-status; code is 0 for it all the same
//	flags    Envoy's %RESPONSE_FLAGS%: "-" or a comma-separated set of short
//	         response flags (UF, UH, UC, …)
//	method   the request's :method, as received
//	details  Envoy's %RESPONSE_CODE_DETAILS%: "via_upstream" for an
//	         application's response, otherwise the reason of the local reply.
//	         Free text; it is last because it is the only field that may
//	         contain a ";"
//
// Values, as the pinned proxy writes them (//agent/test/mtlspool
// TestBegunRequestIsNotReplayed asserts each):
//
//	200;-;GET;via_upstream
//	503;-;POST;via_upstream
//	0;UF;POST;upstream_reset_before_response_started{remote_connection_failure|delayed_connect_error:_Connection_refused}
//	0;UH;POST;no_healthy_upstream
//	0;NC;POST;cluster_not_found
//	0;UC;POST;upstream_reset_before_response_started{connection_termination}
//	0;UR;POST;upstream_reset_before_response_started{remote_reset}
//	0;UR;POST;upstream_reset_before_response_started{remote_refused_stream_reset}
//
// The order is for the reader, outcomeRetriableRegex: what decides first comes
// first, so that the regex shares its prefixes (see the size limit there).
//
// This is a WIRE CONTRACT between agent versions: a caller of one release
// reads what a destination of another wrote. A caller retries nothing on a
// response whose header it does not recognise (or that has none), so a change
// must keep old values meaningful to old callers: add flags to the caller's
// sets, never repurpose a field, and use a new header name for a new grammar.
//
// # Who touches it
//
//   - the inbound route stamps it, overwriting anything the application set
//     (buildInboundRouteConfiguration);
//   - every caller-side route to a mesh cluster matches it for retry and then
//     removes it from the response it forwards (outboundRetryPolicy,
//     stripOutcomeHeader), so it reaches neither the client application nor,
//     at the edge, the outside. Envoy decides the retry before it finalises
//     the response headers, so the removal does not change the decision.
//
// A REQUEST header of this name means nothing: retriable_headers are matched
// against the response, and the destination overwrites whatever an application
// echoes back.
const OutcomeHeader = "x-aether-outcome"

// outcomeFormat is the header's value as Envoy's formatter renders it. See the
// grammar above; outcomeRetriableRegex is its only reader.
const outcomeFormat = "%RESPONSE_CODE%;%RESPONSE_FLAGS%;%REQ(:METHOD)%;%RESPONSE_CODE_DETAILS%"

// The response flags and methods the caller's rule knows. They are the rule's
// statement, and what the tests check the regex against; the regex spells them
// by hand (see there). Everything else, and that includes a flag a future
// Envoy adds, makes the response not retriable: the rule fails closed.
var (
	// outcomeNeverBegunFlags: the router gave up before it sent a byte of the
	// request to the application. Each of these is a 503 (or its gRPC
	// equivalent) on the inbound route.
	//
	//	UF  upstream connection failure: the connect was refused or timed out
	//	UH  no healthy upstream host to pick
	//	UO  upstream overflow: the circuit breaker refused the request
	//	NC  the upstream cluster does not exist (the pod's app cluster is
	//	    removed before its listener on teardown)
	//
	// Deliberately absent, though they are also "nothing sent". The inbound
	// route cannot produce LH (only the health-check filter's own probe
	// paths), DO and UDO (no drop_overload is configured) or DF (no DNS).
	// NR is a 404 that another endpoint would answer the same way; RL, RLSE
	// and UAEX are verdicts, not failures; FI is a fault somebody injected to
	// be seen; IH and DPE mean the request itself is bad; OM also resets
	// requests in flight; URX rides on top of whatever the last attempt
	// failed with.
	outcomeNeverBegunFlags = []string{"UF", "UH", "UO", "NC"}

	// outcomeBegunFlags: the request had begun to the application when it
	// failed without a response.
	//
	//	UC  the application's connection was terminated: closed, half-closed
	//	    or reset at the TCP level (all three measured)
	//	UR  the application reset the stream (HTTP/2 RST_STREAM)
	//	LR  the proxy reset the upstream stream itself (not produced in the
	//	    live test)
	//
	// Absent on purpose, so that they are never replayed for any method: UT
	// and DT (a timeout: the application may still be running the request),
	// UPE (the application answered something malformed) and UMSDR.
	outcomeBegunFlags = []string{"UC", "UR", "LR"}

	// outcomeIdempotentMethods may be replayed by definition (RFC 9110,
	// 9.2.2). A method outside this list, including one the proxy has never
	// heard of, is treated as not idempotent.
	outcomeIdempotentMethods = []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"}
)

// outcomeRefusedStreamDetail picks out a stream the application refused with
// RST_STREAM(REFUSED_STREAM), whose details are
// "upstream_reset_before_response_started{remote_refused_stream_reset}".
// RFC 9113, 8.7: such a request was not processed and "can be safely retried",
// whatever its method. The response flag is UR, the same as for a stream the
// application reset after working on it; only the details tell them apart.
//
// It is the shortest piece of that text no other reset reason shares: a
// connect failure's "…Connection_refused" has "n_refused", and
// "local_refused_stream_reset" has "l_refused". Short because every character
// is an instruction of the compiled regex (see the size limit below).
const outcomeRefusedStreamDetail = "e_refused"

// outcomeRetriableRegex is the caller's whole rule about a response: retry on
// another endpoint if and only if the destination's outcome header matches.
// Envoy matches the WHOLE value (RE2 FullMatch), so there are no anchors. Four
// alternatives:
//
//  1. "503;-;…": the application answered 503 itself (the standard "try
//     another endpoint" signal), any method;
//  2. "0;<never-begun flags>;…": a local reply for a request that never
//     began, any method;
//  3. "0;<known flags>;<idempotent method>;…": a local reply for a request
//     that had begun, for an idempotent method;
//  4. "0;<known flags>;<method>;…e_refused…": a local reply for a stream the
//     application refused, any method.
//
// Code 0 is a local reply (see the grammar). No alternative matches any other
// code than 0 and 503, so a 200 and every other application status are never
// retried, and neither is a begun request with a method outside the
// idempotent list (unless the application refused it). A value from an Envoy
// that recorded a local reply's real code would match nothing: the retry
// would stop, not widen, and the live test would say so.
//
// Three things shape its spelling:
//
//   - RE2 has no lookahead, so "none of the begun flags" is written as "every
//     flag is one of the never-begun ones";
//   - Envoy refuses a regex whose compiled RE2 program is larger than 100
//     instructions (re2.max_program_size.error_level), and with it the whole
//     route table. A straightforward spelling of this rule compiles to 233.
//     Hence the field order, the shared prefixes, U[FHO] for UF|UH|UO, and
//     printable-ASCII classes where "[^;]" and "." would each be a UTF-8
//     automaton (a value with anything else in it fails closed);
//   - for the same reason a flag set is "(?:flag,?)+", which also accepts a
//     set written without commas or with a trailing one. Envoy's formatter is
//     the only writer and joins flags with ",", so no such value exists.
//
// //agent/test/envoy_validate and //agent/test/mtlspool load it into the
// pinned proxy; a pin whose RE2 compiles it larger fails there.
const outcomeRetriableRegex = `503;-;[ -~]*` +
	`|0;(?:` +
	`(?:(?:U[FHO]|NC),?)+;[ -~]*` +
	`|(?:(?:U[CFHOR]|NC|LR),?)+;(?:` +
	`(?:GET|HEAD|OPTIONS|TRACE|PUT|DELETE);[ -~]*` +
	`|[!-:<-~]+;[ -~]*` + outcomeRefusedStreamDetail + `[ -~]*` +
	`))`

// outcomeRetriableHeader is the one retriable_headers matcher of the caller's
// retry policy.
func outcomeRetriableHeader() *routev3.HeaderMatcher {
	return &routev3.HeaderMatcher{
		Name: OutcomeHeader,
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{
			MatchPattern: &matcherv3.StringMatcher_SafeRegex{SafeRegex: &matcherv3.RegexMatcher{Regex: outcomeRetriableRegex}},
		}},
	}
}

// inboundOutcomeHeader is what the inbound virtual host adds to every
// response: the outcome, replacing whatever the application put under that
// name.
func inboundOutcomeHeader() *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: OutcomeHeader, Value: outcomeFormat},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

// stripOutcomeHeader makes a caller-side route to a mesh cluster remove the
// outcome header from the response it forwards. It returns r.
func stripOutcomeHeader(r *routev3.Route) *routev3.Route {
	if !slices.Contains(r.ResponseHeadersToRemove, OutcomeHeader) {
		r.ResponseHeadersToRemove = append(r.ResponseHeadersToRemove, OutcomeHeader)
	}
	return r
}

// isMeshBackendCluster reports whether an edge route's backend cluster is a
// mesh service, whose responses come through a destination proxy, and not a
// cleartext Kubernetes Service the edge dials directly (EdgeK8sClusterName).
func isMeshBackendCluster(cluster string) bool {
	return cluster != "" && !strings.HasPrefix(cluster, edgeK8sClusterPrefix)
}
