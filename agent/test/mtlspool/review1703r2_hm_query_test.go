package mtlspool

// Review of #1703, second pass: a service-wide header_mutation and an
// HTTPRoute rule's header_mutation, where one level mutates query
// parameters. Natively (the proxy port) the pinned proxy applies each LEVEL
// as a unit, route first: route.request_mutations, route.query (parse,
// mutate, re-render the path), then the virtual host's request_mutations and
// query (Mutations::mutateRequestHeaders, header_mutation.cc). cap_http at
// 35955573 carried ONE combined config on the route
// (extensionfilter.CombineLevels), whose single Mutations applies every
// request mutation of both levels before any query mutation, and parses the
// query once instead of twice. It now keeps the levels native on a full name.

import (
	"io"
	"net/http"
	"testing"
	"time"

	"aethermesh.dev/common/extensionfilter"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/structpb"
)

func r2Append(key, value string) *mutation_rulesv3.HeaderMutation {
	return &mutation_rulesv3.HeaderMutation{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{Key: key, Value: value}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}}}
}

func r2Query(key, value string) *corev3.KeyValueMutation {
	return &corev3.KeyValueMutation{Append: &corev3.KeyValueAppend{
		Record: &corev3.KeyValuePair{Key: key, Value: structpb.NewStringValue(value)},
	}}
}

func r2GetPath(addr, host, path string) flResp {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	req.Host = host
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return flResp{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return flResp{status: resp.StatusCode, header: resp.Header}
}

// The service's request mutation reads the path; the rule's query mutation
// changes it. Natively the rule's query runs first.
func TestReview1703R2_LiveHeaderMutationRequestVsQueryOrder(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	svc := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		RequestMutations: []*mutation_rulesv3.HeaderMutation{r2Append("x-fl-seen", "%REQ(:PATH)%")},
	}}
	route := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		QueryParameterMutations: []*corev3.KeyValueMutation{r2Query("q", "1")},
		ResponseMutations:       []*mutation_rulesv3.HeaderMutation{r2Append("x-fl-route", "%REQ(x-fl-seen)%")},
	}}
	flLevels(t, r, extensionfilter.HeaderMutationFilterName, flAny(t, svc), flAny(t, route))
	captured := r2GetPath(capture, ppMesh+":8080", "/")
	viaProxy := r2GetPath(proxyPort, ppMesh+":8080", "/")
	t.Logf("MEASURED captured (compiled cap_http): %s", captured)
	t.Logf("MEASURED proxy port (native levels):  %s", viaProxy)
	assert.Equal(t, viaProxy.header.Get("X-Fl-Route"), captured.header.Get("X-Fl-Route"),
		"the service-wide request mutation saw a different :path on the captured path than on the proxy port")
}

// Both levels mutate the query: natively the path is parsed and re-rendered
// twice (decoding each time, never re-encoding), on the captured path once.
func TestReview1703R2_LiveHeaderMutationQueryBothLevels(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	svc := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		QueryParameterMutations: []*corev3.KeyValueMutation{r2Query("v", "2")},
	}}
	route := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		QueryParameterMutations: []*corev3.KeyValueMutation{r2Query("q", "1")},
		ResponseMutations:       []*mutation_rulesv3.HeaderMutation{r2Append("x-fl-route", "%REQ(:PATH)%")},
	}}
	flLevels(t, r, extensionfilter.HeaderMutationFilterName, flAny(t, svc), flAny(t, route))
	captured := r2GetPath(capture, ppMesh+":8080", "/?a=%2541")
	viaProxy := r2GetPath(proxyPort, ppMesh+":8080", "/?a=%2541")
	t.Logf("MEASURED captured (compiled cap_http): %s", captured)
	t.Logf("MEASURED proxy port (native levels):  %s", viaProxy)
	assert.Equal(t, viaProxy.header.Get("X-Fl-Route"), captured.header.Get("X-Fl-Route"),
		"the backend was sent a different :path on the captured path than on the proxy port")
}
