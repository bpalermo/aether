package mtlspool

// The route side of cap_http, live: what the captured path answers must be
// what the proxy port (the loopback listener, out_http: the loopback table's
// virtual hosts at their native levels) answers, under the full name and the
// short name, for
// the GAMMA rule shapes the compiled table carries on its routes: a header
// match, a weighted split, and a rule imported from a peer cluster. The unit
// equivalence gate compares the configuration; this compares the proxy's
// behaviour.

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/require"
)

func TestOneHCM_LiveRulesAnswerAsOnTheProxyPort(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	mark := func(v string) *proxy.GammaHeaderMutation {
		return &proxy.GammaHeaderMutation{AddResponse: []proxy.GammaHeaderKV{{Name: "x-fl-route", Value: v}}}
	}
	r.c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {
		// A header match: only a request that carries it takes the rule.
		{Matches: []proxy.GammaMatch{{Prefix: "/", Headers: []proxy.GammaHeaderMatch{{Name: "x-pick", Value: "hdr"}}}}, HeaderMutation: mark("header")},
		// A weighted split over two backends of the mesh.
		{
			Matches: []proxy.GammaMatch{{Prefix: "/"}},
			Backends: []proxy.GammaBackend{
				{Service: "demo/other", Cluster: "other.demo.aether.internal", Port: 8080, Weight: 1},
				{Service: ppElsewhere, Cluster: "multi.elsewhere.aether.internal", Port: 8080, Weight: 1},
			},
			HeaderMutation: mark("weighted"),
		},
	}})
	r.c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{"demo/other": {
		{Imported: true, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: mark("imported")},
	}})
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	r.publish()
	time.Sleep(3 * time.Second)

	get := func(addr, host string, pick bool) flResp {
		resp := flGetWith(addr, host, map[string]string{})
		if pick {
			resp = flGetWith(addr, host, map[string]string{"x-pick": "hdr"})
		}
		return resp
	}
	for _, tc := range []struct {
		what, host string
		pick       bool
		want       string
	}{
		{"a header-matched rule, full name", ppMesh + ":8080", true, "header"},
		{"a header-matched rule, short name", "multi:8080", true, "header"},
		{"a weighted rule, full name", ppMesh + ":8080", false, "weighted"},
		{"a weighted rule, short name", "multi:8080", false, "weighted"},
		{"an imported rule", "other.demo.aether.internal:8080", false, "imported"},
	} {
		for i := 0; i < 4; i++ {
			captured := get(capture, tc.host, tc.pick)
			viaProxy := get(proxyPort, tc.host, tc.pick)
			t.Logf("MEASURED %s: captured %s | proxy port %s", tc.what, captured, viaProxy)
			require.NoError(t, captured.err)
			require.NoError(t, viaProxy.err)
			require.Equal(t, viaProxy.status, captured.status, tc.what)
			require.Equal(t, 200, captured.status, tc.what)
			require.Equal(t, tc.want, captured.header.Get("X-Fl-Route"), "%s: the captured path ran another rule", tc.what)
			require.Equal(t, viaProxy.header.Get("X-Fl-Route"), captured.header.Get("X-Fl-Route"), tc.what)
		}
	}
}

// flGetWith is flGet with request headers.
func flGetWith(addr, host string, headers map[string]string) flResp {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
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
