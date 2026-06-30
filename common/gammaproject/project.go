// Package gammaproject projects Gateway API HTTPRoute/GRPCRoute rules attached to a
// Service into the data-plane GAMMA route model (proposal 018), as the
// registryv1.GammaRoute PROTO. It is the single source of truth shared by the agent's
// GAMMA reconciler (which converts the proto to its in-memory form) AND the registrar's
// cross-cluster export controller (proposal 026 EM1c, which writes the proto to the
// registry). Keeping one projector avoids drift between what a cluster routes locally
// and what it exports for peers to import. Pure: no agent/registrar internals.
package gammaproject

import (
	"fmt"
	"time"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/bpalermo/aether/common/extensionfilter"
	"github.com/bpalermo/aether/common/referencegrant"
	"github.com/bpalermo/aether/common/serviceref"
	"google.golang.org/protobuf/types/known/durationpb"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// ServiceParent is one Service a route attaches to (parentRef kind=Service): the
// route-target "<ns>/<svc>" key and the parentRef port (0 = unset).
type ServiceParent struct {
	Key  string
	Port uint32
}

// ServiceParents returns the Service parentRefs a route attaches to (kind=Service,
// core group), as route-target keys + ports. Shared by the agent reconciler and the
// registrar export controller so both agree on what a route targets.
func ServiceParents(refs []gatewayv1.ParentReference, routeNamespace string) []ServiceParent {
	var parents []ServiceParent
	for _, p := range refs {
		if p.Group != nil && string(*p.Group) != "" {
			continue
		}
		if p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		ns := routeNamespace
		if p.Namespace != nil && string(*p.Namespace) != "" {
			ns = string(*p.Namespace)
		}
		var port uint32
		if p.Port != nil {
			port = uint32(*p.Port)
		}
		parents = append(parents, ServiceParent{Key: serviceref.New(ns, string(p.Name)).Key(), Port: port})
	}
	return parents
}

// ProjectHTTPRule projects one HTTPRoute rule into a GammaRoute proto. Backends are
// resolved to namespace-qualified "<ns>/<svc>" keys + data-plane cluster names
// (<svc>.<meshDomain>); ungranted cross-namespace backends are dropped. httpFilters
// resolves a route's ExtensionRef escape-hatch filters (proposal 025), keyed by
// "<ns>/<name>"; nil/empty when none.
func ProjectHTTPRule(rule gatewayv1.HTTPRouteRule, routeNamespace, routeKind, meshDomain string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configprotov1.HTTPFilterSpec) *registryv1.GammaRoute {
	gr := &registryv1.GammaRoute{}
	if rule.Timeouts != nil && rule.Timeouts.Request != nil {
		if d, err := time.ParseDuration(string(*rule.Timeouts.Request)); err == nil && d > 0 {
			gr.Timeout = durationpb.New(d)
		}
	}
	gr.Backends = projectBackends(rule.BackendRefs, routeNamespace, routeKind, meshDomain, grants)
	for _, m := range rule.Matches {
		gm := &registryv1.GammaMatch{}
		if m.Path != nil && m.Path.Value != nil {
			if m.Path.Type != nil && *m.Path.Type == gatewayv1.PathMatchExact {
				gm.Exact = *m.Path.Value
			} else {
				gm.Prefix = *m.Path.Value
			}
		}
		for _, h := range m.Headers {
			gm.Headers = append(gm.Headers, &registryv1.GammaHeaderMatch{Name: string(h.Name), Value: h.Value})
		}
		gr.Matches = append(gr.Matches, gm)
	}
	gr.HeaderMutation = httpHeaderMutation(rule.Filters)
	gr.Redirect = httpRedirect(rule.Filters)
	gr.UrlRewrite = httpURLRewrite(rule.Filters)
	for _, f := range rule.Filters {
		if f.Type == gatewayv1.HTTPRouteFilterExtensionRef {
			if ef, ok := resolveExtensionFilter(f.ExtensionRef, routeNamespace, httpFilters); ok {
				gr.ExtensionFilters = append(gr.ExtensionFilters, ef)
			}
		}
	}
	return gr
}

// ProjectGRPCRule projects one GRPCRoute rule. gRPC rides HTTP/2 as POST
// /<service>/<method>, so a method match becomes a path match (svc+method = exact
// /svc/method; svc-only = prefix /svc/; regex for RegularExpression). No per-rule timeout.
func ProjectGRPCRule(rule gatewayv1.GRPCRouteRule, routeNamespace, routeKind, meshDomain string, grants []gatewayv1beta1.ReferenceGrant, httpFilters map[string]*configprotov1.HTTPFilterSpec) *registryv1.GammaRoute {
	gr := &registryv1.GammaRoute{}
	gr.Backends = projectGRPCBackends(rule.BackendRefs, routeNamespace, routeKind, meshDomain, grants)
	for _, m := range rule.Matches {
		gm := &registryv1.GammaMatch{}
		if m.Method != nil {
			svc, method := "", ""
			if m.Method.Service != nil {
				svc = *m.Method.Service
			}
			if m.Method.Method != nil {
				method = *m.Method.Method
			}
			matchType := gatewayv1.GRPCMethodMatchExact
			if m.Method.Type != nil {
				matchType = *m.Method.Type
			}
			switch matchType {
			case gatewayv1.GRPCMethodMatchExact:
				if svc != "" && method != "" {
					gm.Exact = fmt.Sprintf("/%s/%s", svc, method)
				} else if svc != "" {
					gm.Prefix = fmt.Sprintf("/%s/", svc)
				}
			case gatewayv1.GRPCMethodMatchRegularExpression:
				svcPart := svc
				if svcPart == "" {
					svcPart = "[^/]+"
				}
				methodPart := method
				if methodPart == "" {
					methodPart = "[^/]+"
				}
				gm.Regex = fmt.Sprintf("/%s/%s", svcPart, methodPart)
			}
		}
		for _, h := range m.Headers {
			gm.Headers = append(gm.Headers, &registryv1.GammaHeaderMatch{Name: string(h.Name), Value: h.Value})
		}
		gr.Matches = append(gr.Matches, gm)
	}
	gr.HeaderMutation = grpcHeaderMutation(rule.Filters)
	for _, f := range rule.Filters {
		if f.Type == gatewayv1.GRPCRouteFilterExtensionRef {
			if ef, ok := resolveExtensionFilter(f.ExtensionRef, routeNamespace, httpFilters); ok {
				gr.ExtensionFilters = append(gr.ExtensionFilters, ef)
			}
		}
	}
	return gr
}

func projectBackends(refs []gatewayv1.HTTPBackendRef, routeNamespace, routeKind, meshDomain string, grants []gatewayv1beta1.ReferenceGrant) []*registryv1.GammaBackend {
	var out []*registryv1.GammaBackend
	for _, b := range refs {
		if be := projectBackend(b.BackendObjectReference, b.Weight, routeNamespace, routeKind, meshDomain, grants); be != nil {
			out = append(out, be)
		}
	}
	return out
}

func projectGRPCBackends(refs []gatewayv1.GRPCBackendRef, routeNamespace, routeKind, meshDomain string, grants []gatewayv1beta1.ReferenceGrant) []*registryv1.GammaBackend {
	var out []*registryv1.GammaBackend
	for _, b := range refs {
		if be := projectBackend(b.BackendObjectReference, b.Weight, routeNamespace, routeKind, meshDomain, grants); be != nil {
			out = append(out, be)
		}
	}
	return out
}

func projectBackend(ref gatewayv1.BackendObjectReference, weight *int32, routeNamespace, routeKind, meshDomain string, grants []gatewayv1beta1.ReferenceGrant) *registryv1.GammaBackend {
	if ref.Group != nil && string(*ref.Group) != "" {
		return nil
	}
	if ref.Kind != nil && string(*ref.Kind) != "Service" {
		return nil
	}
	name := string(ref.Name)
	// Drop ungranted cross-namespace backends (RefNotPermitted): removed from the data
	// plane, but the rest of the route still applies.
	if !backendPermitted(ref.Namespace, routeNamespace, routeKind, name, grants) {
		return nil
	}
	w := uint32(1)
	if weight != nil {
		w = uint32(*weight)
	}
	key := backendServiceKey(ref.Namespace, routeNamespace, name)
	return &registryv1.GammaBackend{Service: key, Cluster: serviceClusterName(key, meshDomain), Weight: w}
}

// serviceClusterName builds a backend's data-plane cluster name "<svc>.<meshDomain>"
// from its "<ns>/<svc>" key (= proxy.ServiceClusterName, kept in lockstep).
func serviceClusterName(serviceKey, meshDomain string) string {
	ref, ok := serviceref.ParseKey(serviceKey)
	if !ok {
		return ""
	}
	return ref.FQDN(meshDomain)
}

func backendServiceKey(backendNamespace *gatewayv1.Namespace, routeNamespace, name string) string {
	ns := routeNamespace
	if bn := derefBackendNamespace(backendNamespace); bn != "" {
		ns = bn
	}
	return serviceref.New(ns, name).Key()
}

func backendPermitted(backendNamespace *gatewayv1.Namespace, routeNamespace, routeKind, name string, grants []gatewayv1beta1.ReferenceGrant) bool {
	ns := derefBackendNamespace(backendNamespace)
	if !referencegrant.CrossNamespace(ns, routeNamespace) {
		return true
	}
	return referencegrant.PermitsBackend(grants, gatewayv1.GroupName, routeKind, routeNamespace, ns, name)
}

func derefBackendNamespace(ns *gatewayv1.Namespace) string {
	if ns == nil {
		return ""
	}
	return string(*ns)
}

// resolveExtensionFilter resolves a route's ExtensionRef (proposal 025) to a proto
// ExtensionFilter, or (nil, false) when it is not an in-namespace, allow-listed,
// ROUTE-scope HTTPFilter with a typed_config. ExtensionRef is same-namespace (a
// LocalObjectReference), so no ReferenceGrant applies.
func resolveExtensionFilter(ref *gatewayv1.LocalObjectReference, routeNamespace string, httpFilters map[string]*configprotov1.HTTPFilterSpec) (*registryv1.ExtensionFilter, bool) {
	if ref == nil ||
		string(ref.Group) != configapisv1.GroupVersion.Group ||
		string(ref.Kind) != configapisv1.HTTPFilterKind {
		return nil, false
	}
	spec, ok := httpFilters[routeNamespace+"/"+string(ref.Name)]
	if !ok || spec == nil {
		return nil, false
	}
	if spec.GetScope() == configprotov1.HTTPFilterSpec_SCOPE_CHAIN {
		return nil, false // chain scope deferred to a later milestone
	}
	name := spec.GetFilter()
	if !extensionfilter.Allowed(name) || spec.GetTypedConfig() == nil {
		return nil, false
	}
	return &registryv1.ExtensionFilter{Name: name, Config: spec.GetTypedConfig()}, true
}

func httpHeaderMutation(filters []gatewayv1.HTTPRouteFilter) *registryv1.GammaHeaderMutation {
	var m *registryv1.GammaHeaderMutation
	ensure := func() {
		if m == nil {
			m = &registryv1.GammaHeaderMutation{}
		}
	}
	for _, f := range filters {
		switch f.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier == nil {
				continue
			}
			ensure()
			m.SetRequest = appendKV(m.SetRequest, f.RequestHeaderModifier.Set)
			m.AddRequest = appendKV(m.AddRequest, f.RequestHeaderModifier.Add)
			m.RemoveRequest = append(m.RemoveRequest, f.RequestHeaderModifier.Remove...)
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier == nil {
				continue
			}
			ensure()
			m.SetResponse = appendKV(m.SetResponse, f.ResponseHeaderModifier.Set)
			m.AddResponse = appendKV(m.AddResponse, f.ResponseHeaderModifier.Add)
			m.RemoveResponse = append(m.RemoveResponse, f.ResponseHeaderModifier.Remove...)
		}
	}
	return m
}

func grpcHeaderMutation(filters []gatewayv1.GRPCRouteFilter) *registryv1.GammaHeaderMutation {
	var m *registryv1.GammaHeaderMutation
	ensure := func() {
		if m == nil {
			m = &registryv1.GammaHeaderMutation{}
		}
	}
	for _, f := range filters {
		switch f.Type {
		case gatewayv1.GRPCRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier == nil {
				continue
			}
			ensure()
			m.SetRequest = appendKV(m.SetRequest, f.RequestHeaderModifier.Set)
			m.AddRequest = appendKV(m.AddRequest, f.RequestHeaderModifier.Add)
			m.RemoveRequest = append(m.RemoveRequest, f.RequestHeaderModifier.Remove...)
		case gatewayv1.GRPCRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier == nil {
				continue
			}
			ensure()
			m.SetResponse = appendKV(m.SetResponse, f.ResponseHeaderModifier.Set)
			m.AddResponse = appendKV(m.AddResponse, f.ResponseHeaderModifier.Add)
			m.RemoveResponse = append(m.RemoveResponse, f.ResponseHeaderModifier.Remove...)
		}
	}
	return m
}

func appendKV(dst []*registryv1.GammaHeaderKv, src []gatewayv1.HTTPHeader) []*registryv1.GammaHeaderKv {
	for _, h := range src {
		dst = append(dst, &registryv1.GammaHeaderKv{Name: string(h.Name), Value: h.Value})
	}
	return dst
}

func httpRedirect(filters []gatewayv1.HTTPRouteFilter) *registryv1.GammaRedirect {
	for _, f := range filters {
		if f.Type != gatewayv1.HTTPRouteFilterRequestRedirect || f.RequestRedirect == nil {
			continue
		}
		rd := f.RequestRedirect
		r := &registryv1.GammaRedirect{}
		if rd.Scheme != nil {
			r.Scheme = *rd.Scheme
		}
		if rd.Hostname != nil {
			r.Hostname = string(*rd.Hostname)
		}
		if rd.Port != nil {
			r.Port = uint32(*rd.Port)
		}
		if rd.StatusCode != nil {
			r.StatusCode = int32(*rd.StatusCode)
		}
		if rd.Path != nil {
			r.PathType, r.PathValue = pathModifier(rd.Path)
		}
		return r
	}
	return nil
}

func httpURLRewrite(filters []gatewayv1.HTTPRouteFilter) *registryv1.GammaUrlRewrite {
	for _, f := range filters {
		if f.Type != gatewayv1.HTTPRouteFilterURLRewrite || f.URLRewrite == nil {
			continue
		}
		rw := f.URLRewrite
		r := &registryv1.GammaUrlRewrite{}
		if rw.Hostname != nil {
			r.Hostname = string(*rw.Hostname)
		}
		if rw.Path != nil {
			r.PathType, r.PathValue = pathModifier(rw.Path)
		}
		return r
	}
	return nil
}

func pathModifier(p *gatewayv1.HTTPPathModifier) (pathType, pathValue string) {
	switch p.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		if p.ReplaceFullPath != nil {
			return "ReplaceFullPath", *p.ReplaceFullPath
		}
	case gatewayv1.PrefixMatchHTTPPathModifier:
		if p.ReplacePrefixMatch != nil {
			return "ReplacePrefixMatch", *p.ReplacePrefixMatch
		}
	}
	return "", ""
}
