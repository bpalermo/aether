package proxy

import (
	"slices"
	"strings"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
)

// The request-begun mark (aether#1641).
//
// A destination proxy answers 503 in two very different situations, and the
// caller's retry policy (outboundRetryPolicy: retriable-status-codes [503])
// could not tell them apart:
//
//   - it never reached its application: the connect was refused or timed out
//     (response flag UF), no host was healthy (UH), the pool overflowed (UO),
//     the app cluster was not there (NC). Replaying the request on another
//     endpoint is safe whatever its method, and is what keeps a pod roll
//     hitless;
//   - it had begun sending the request to the application, and the
//     application's connection then closed or reset before any response (UC,
//     UR, LR). The application may have run the request. Replaying a POST
//     runs it twice.
//
// The status code stays 503 in both. The destination marks the second kind
// instead, and only for a method that is not idempotent: a GET, HEAD, OPTIONS,
// TRACE, PUT or DELETE may be replayed by definition (RFC 9110, 9.2.2), so it
// is left unmarked and the caller moves it to another endpoint exactly as it
// did before. That is also what keeps the HTTP/1.1 keep-alive race invisible
// for those methods (an application closing an idle connection at the instant
// the proxy reuses it is a UC, too).
//
// The mark is the response header x-envoy-ratelimited, because that is the one
// response header Envoy's retry state treats as "do not retry this response":
// RetryStateImpl::wouldRetryFromHeaders returns NoRetry for a response that
// carries it, before it looks at the status code, unless the policy lists
// envoy-ratelimited (ours does not). Every other response-driven retry
// condition can only ADD a reason to retry, and they are OR-ed, so a header of
// our own would have to replace retriable-status-codes with a retriable-headers
// matcher that also encodes the status: a caller with that policy would stop
// retrying every 503 of a destination that does not stamp the header yet, for
// the length of an upgrade or a rollback. With Envoy's own veto the caller's
// policy does not change at all, so a caller of any version honours the mark
// and an unmarked 503 (an older destination) is retried as it always was.
// Measured on the pinned proxy by //agent/test/mtlspool
// TestBegunRequestIsNotReplayed.
//
// Three places handle the header:
//
//   - the inbound connection manager adds it (inboundRequestBegunLocalReply);
//   - the inbound route removes it from what the APPLICATION answered
//     (buildInboundRouteConfiguration), so an application cannot use it to
//     switch its own 503s out of the mesh retry: between proxies the header
//     means one thing only. The route's removal runs before the local-reply
//     mapper adds, so it never removes the mark itself;
//   - every caller-side route to a mesh cluster removes it after the retry
//     decision (dropRequestBegunHeader), so it reaches neither the client
//     application nor, at the edge, the outside.
const requestBegunHeader = "x-envoy-ratelimited"

// requestBegunFlags are the response flags of a 503 the router synthesised
// after it had started sending the request upstream: the connection was
// terminated (UC), the stream was reset by the application (UR) or by the proxy
// itself (LR, which includes an HTTP/1 application half-closing early). The
// connect-phase flags (UF, UH, UO, NC) are deliberately absent.
var requestBegunFlags = []string{"UC", "UR", "LR"}

// idempotentMethodRegex matches the methods a caller may replay without asking
// (RFC 9110, 9.2.2). Anything else, including a method the proxy has never
// heard of, is treated as not idempotent.
const idempotentMethodRegex = `^(GET|HEAD|OPTIONS|TRACE|PUT|DELETE)$`

// inboundRequestBegunLocalReply is the inbound connection manager's
// local_reply_config: it adds the request-begun mark to a locally generated
// reply whose response flags say the request had begun to the application, when
// the request's method is not idempotent. It changes neither the status code
// nor the body. A response the application itself sent is not a local reply and
// is never touched.
func inboundRequestBegunLocalReply() *http_connection_managerv3.LocalReplyConfig {
	begun := &accesslogv3.AccessLogFilter{FilterSpecifier: &accesslogv3.AccessLogFilter_ResponseFlagFilter{
		ResponseFlagFilter: &accesslogv3.ResponseFlagFilter{Flags: slices.Clone(requestBegunFlags)},
	}}
	notIdempotent := &accesslogv3.AccessLogFilter{FilterSpecifier: &accesslogv3.AccessLogFilter_HeaderFilter{
		HeaderFilter: &accesslogv3.HeaderFilter{Header: &routev3.HeaderMatcher{
			Name: ":method",
			HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: &matcherv3.StringMatcher{
				MatchPattern: &matcherv3.StringMatcher_SafeRegex{SafeRegex: &matcherv3.RegexMatcher{Regex: idempotentMethodRegex}},
			}},
			InvertMatch: true,
		}},
	}}
	return &http_connection_managerv3.LocalReplyConfig{
		Mappers: []*http_connection_managerv3.ResponseMapper{{
			// The flag filter comes first: an and_filter stops at the first
			// filter that does not match, so the method is only looked at for
			// a reply the router generated for a request it had read.
			Filter: &accesslogv3.AccessLogFilter{FilterSpecifier: &accesslogv3.AccessLogFilter_AndFilter{
				AndFilter: &accesslogv3.AndFilter{Filters: []*accesslogv3.AccessLogFilter{begun, notIdempotent}},
			}},
			HeadersToAdd: []*corev3.HeaderValueOption{{
				Header:       &corev3.HeaderValue{Key: requestBegunHeader, Value: "true"},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}},
		}},
	}
}

// dropRequestBegunHeader makes a caller-side route to a mesh cluster remove the
// request-begun mark from the response it forwards. The router decides whether
// to retry before it finalises the response headers, so the removal does not
// change that decision. It returns r.
func dropRequestBegunHeader(r *routev3.Route) *routev3.Route {
	if !slices.Contains(r.ResponseHeadersToRemove, requestBegunHeader) {
		r.ResponseHeadersToRemove = append(r.ResponseHeadersToRemove, requestBegunHeader)
	}
	return r
}

// isMeshBackendCluster reports whether an edge route's backend cluster is a
// mesh service, whose responses come through a destination proxy, and not a
// cleartext Kubernetes Service the edge dials directly (EdgeK8sClusterName).
// Only a destination proxy sets the request-begun mark; a header of that name
// from a backend outside the mesh is that backend's own and is left alone.
func isMeshBackendCluster(cluster string) bool {
	return cluster != "" && !strings.HasPrefix(cluster, edgeK8sClusterPrefix)
}
