package proxy

import (
	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

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
