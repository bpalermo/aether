package proxy

import (
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// ToConfigProjection converts a service's in-memory GAMMA rules to the cross-cluster
// wire form (proposal 026): the exporting (authoritative) cluster writes this to the
// shared registry, origin-stamped + versioned, for consuming clusters to import. The
// inverse is FromConfigProjection; together they round-trip a GammaRoute set.
func ToConfigProjection(service, originCluster, version string, rules []GammaRoute) *registryv1.ServiceConfigProjection {
	p := &registryv1.ServiceConfigProjection{
		Service:       service,
		OriginCluster: originCluster,
		Version:       version,
	}
	for i := range rules {
		p.Routes = append(p.Routes, gammaRouteToProto(rules[i]))
	}
	return p
}

// FromConfigProjection converts an imported projection back to in-memory GAMMA rules
// the consumer feeds into SetServiceRoutes (it then builds vhosts exactly as the local
// path does). nil-safe.
func FromConfigProjection(p *registryv1.ServiceConfigProjection) []GammaRoute {
	if p == nil {
		return nil
	}
	rules := make([]GammaRoute, 0, len(p.GetRoutes()))
	for _, r := range p.GetRoutes() {
		rules = append(rules, GammaRouteFromProto(r))
	}
	return rules
}

func gammaRouteToProto(r GammaRoute) *registryv1.GammaRoute {
	out := &registryv1.GammaRoute{Timeout: r.Timeout}
	for _, m := range r.Matches {
		pm := &registryv1.GammaMatch{Prefix: m.Prefix, Exact: m.Exact, Regex: m.Regex}
		for _, h := range m.Headers {
			pm.Headers = append(pm.Headers, &registryv1.GammaHeaderMatch{Name: h.Name, Value: h.Value})
		}
		out.Matches = append(out.Matches, pm)
	}
	for _, b := range r.Backends {
		out.Backends = append(out.Backends, &registryv1.GammaBackend{Service: b.Service, Cluster: b.Cluster, Weight: b.Weight})
	}
	if r.HeaderMutation != nil {
		out.HeaderMutation = headerMutationToProto(r.HeaderMutation)
	}
	if r.Redirect != nil {
		out.Redirect = &registryv1.GammaRedirect{
			Scheme: r.Redirect.Scheme, Hostname: r.Redirect.Hostname, Port: r.Redirect.Port,
			StatusCode: int32(r.Redirect.StatusCode), PathType: r.Redirect.PathType,
			PathValue: r.Redirect.PathValue, ListenerPort: r.Redirect.ListenerPort,
		}
	}
	if r.URLRewrite != nil {
		out.UrlRewrite = &registryv1.GammaUrlRewrite{Hostname: r.URLRewrite.Hostname, PathType: r.URLRewrite.PathType, PathValue: r.URLRewrite.PathValue}
	}
	for _, ef := range r.ExtensionFilters {
		out.ExtensionFilters = append(out.ExtensionFilters, &registryv1.ExtensionFilter{Name: ef.Name, Config: ef.Config})
	}
	return out
}

// GammaRouteFromProto converts a registryv1.GammaRoute proto (produced by the shared
// gammaproject projector, locally or imported from a peer) into the agent's in-memory
// GammaRoute the cache consumes.
func GammaRouteFromProto(r *registryv1.GammaRoute) GammaRoute {
	out := GammaRoute{Timeout: r.GetTimeout()}
	for _, m := range r.GetMatches() {
		gm := GammaMatch{Prefix: m.GetPrefix(), Exact: m.GetExact(), Regex: m.GetRegex()}
		for _, h := range m.GetHeaders() {
			gm.Headers = append(gm.Headers, GammaHeaderMatch{Name: h.GetName(), Value: h.GetValue()})
		}
		out.Matches = append(out.Matches, gm)
	}
	for _, b := range r.GetBackends() {
		out.Backends = append(out.Backends, GammaBackend{Service: b.GetService(), Cluster: b.GetCluster(), Weight: b.GetWeight()})
	}
	if m := r.GetHeaderMutation(); m != nil {
		out.HeaderMutation = headerMutationFromProto(m)
	}
	if rd := r.GetRedirect(); rd != nil {
		out.Redirect = &GammaRedirect{
			Scheme: rd.GetScheme(), Hostname: rd.GetHostname(), Port: rd.GetPort(),
			StatusCode: int(rd.GetStatusCode()), PathType: rd.GetPathType(),
			PathValue: rd.GetPathValue(), ListenerPort: rd.GetListenerPort(),
		}
	}
	if u := r.GetUrlRewrite(); u != nil {
		out.URLRewrite = &GammaURLRewrite{Hostname: u.GetHostname(), PathType: u.GetPathType(), PathValue: u.GetPathValue()}
	}
	for _, ef := range r.GetExtensionFilters() {
		out.ExtensionFilters = append(out.ExtensionFilters, ExtensionFilter{Name: ef.GetName(), Config: ef.GetConfig()})
	}
	return out
}

func headerMutationToProto(m *GammaHeaderMutation) *registryv1.GammaHeaderMutation {
	kv := func(in []GammaHeaderKV) []*registryv1.GammaHeaderKv {
		var out []*registryv1.GammaHeaderKv
		for _, e := range in {
			out = append(out, &registryv1.GammaHeaderKv{Name: e.Name, Value: e.Value})
		}
		return out
	}
	return &registryv1.GammaHeaderMutation{
		SetRequest: kv(m.SetRequest), AddRequest: kv(m.AddRequest), RemoveRequest: m.RemoveRequest,
		SetResponse: kv(m.SetResponse), AddResponse: kv(m.AddResponse), RemoveResponse: m.RemoveResponse,
	}
}

func headerMutationFromProto(m *registryv1.GammaHeaderMutation) *GammaHeaderMutation {
	kv := func(in []*registryv1.GammaHeaderKv) []GammaHeaderKV {
		var out []GammaHeaderKV
		for _, e := range in {
			out = append(out, GammaHeaderKV{Name: e.GetName(), Value: e.GetValue()})
		}
		return out
	}
	return &GammaHeaderMutation{
		SetRequest: kv(m.GetSetRequest()), AddRequest: kv(m.GetAddRequest()), RemoveRequest: m.GetRemoveRequest(),
		SetResponse: kv(m.GetSetResponse()), AddResponse: kv(m.GetAddResponse()), RemoveResponse: m.GetRemoveResponse(),
	}
}
