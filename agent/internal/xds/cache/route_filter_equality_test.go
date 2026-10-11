package cache

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
)

// TestRedirectAndRewriteEquality holds the two filter comparisons to content
// equality, through both route models that carry the filters: an edge Route
// (equalRoute) and a mesh GammaRoute (equalGammaRoute). The reconcilers
// allocate a new struct on every pass, so a comparison by pointer would rebuild
// the snapshot on every pass, and one that ignored a field would drop a real
// change.
func TestRedirectAndRewriteEquality(t *testing.T) {
	redirect := func(mut func(*proxy.GammaRedirect)) *proxy.GammaRedirect {
		r := &proxy.GammaRedirect{Scheme: "https", Hostname: "example.org", Port: 443, StatusCode: 301, PathType: "ReplaceFullPath", PathValue: "/new"}
		if mut != nil {
			mut(r)
		}
		return r
	}
	rewrite := func(mut func(*proxy.GammaURLRewrite)) *proxy.GammaURLRewrite {
		r := &proxy.GammaURLRewrite{Hostname: "example.org", PathType: "ReplacePrefixMatch", PathValue: "/v2"}
		if mut != nil {
			mut(r)
		}
		return r
	}

	redirects := []struct {
		name string
		a, b *proxy.GammaRedirect
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and set", nil, redirect(nil), false},
		{"set and nil", redirect(nil), nil, false},
		{"same content, two allocations", redirect(nil), redirect(nil), true},
		{"scheme", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.Scheme = "http" }), false},
		{"hostname", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.Hostname = "example.net" }), false},
		{"port", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.Port = 8443 }), false},
		{"status code", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.StatusCode = 302 }), false},
		{"path type", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.PathType = "ReplacePrefixMatch" }), false},
		{"path value", redirect(nil), redirect(func(r *proxy.GammaRedirect) { r.PathValue = "/other" }), false},
	}
	for _, tc := range redirects {
		t.Run("redirect/"+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, equalGammaRedirect(tc.a, tc.b))
			assert.Equal(t, tc.want, equalRoute(Route{Redirect: tc.a}, Route{Redirect: tc.b}), "edge Route")
			assert.Equal(t, tc.want, equalGammaRoute(proxy.GammaRoute{Redirect: tc.a}, proxy.GammaRoute{Redirect: tc.b}), "mesh GammaRoute")
		})
	}

	rewrites := []struct {
		name string
		a, b *proxy.GammaURLRewrite
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and set", nil, rewrite(nil), false},
		{"set and nil", rewrite(nil), nil, false},
		{"same content, two allocations", rewrite(nil), rewrite(nil), true},
		{"hostname", rewrite(nil), rewrite(func(r *proxy.GammaURLRewrite) { r.Hostname = "example.net" }), false},
		{"path type", rewrite(nil), rewrite(func(r *proxy.GammaURLRewrite) { r.PathType = "ReplaceFullPath" }), false},
		{"path value", rewrite(nil), rewrite(func(r *proxy.GammaURLRewrite) { r.PathValue = "/v3" }), false},
	}
	for _, tc := range rewrites {
		t.Run("rewrite/"+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, equalGammaURLRewrite(tc.a, tc.b))
			assert.Equal(t, tc.want, equalRoute(Route{URLRewrite: tc.a}, Route{URLRewrite: tc.b}), "edge Route")
			assert.Equal(t, tc.want, equalGammaRoute(proxy.GammaRoute{URLRewrite: tc.a}, proxy.GammaRoute{URLRewrite: tc.b}), "mesh GammaRoute")
		})
	}
}
