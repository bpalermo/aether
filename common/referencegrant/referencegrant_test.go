package referencegrant

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// grant builds a ReferenceGrant in toNamespace with the given from + to entries.
func grant(toNamespace string, from []gatewayv1.ReferenceGrantFrom, to []gatewayv1.ReferenceGrantTo) gatewayv1beta1.ReferenceGrant {
	return gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: toNamespace, Name: "g"},
		Spec:       gatewayv1beta1.ReferenceGrantSpec{From: from, To: to},
	}
}

func from(group, kind, ns string) gatewayv1.ReferenceGrantFrom {
	return gatewayv1.ReferenceGrantFrom{
		Group:     gatewayv1.Group(group),
		Kind:      gatewayv1.Kind(kind),
		Namespace: gatewayv1.Namespace(ns),
	}
}

func to(group, kind string, name *string) gatewayv1.ReferenceGrantTo {
	t := gatewayv1.ReferenceGrantTo{Group: gatewayv1.Group(group), Kind: gatewayv1.Kind(kind)}
	if name != nil {
		on := gatewayv1.ObjectName(*name)
		t.Name = &on
	}
	return t
}

func strp(s string) *string { return &s }

func TestCrossNamespace(t *testing.T) {
	cases := []struct {
		name             string
		backendNamespace string
		routeNamespace   string
		want             bool
	}{
		{"unset is same-namespace", "", "ns-a", false},
		{"equal is same-namespace", "ns-a", "ns-a", false},
		{"different is cross-namespace", "ns-b", "ns-a", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CrossNamespace(c.backendNamespace, c.routeNamespace); got != c.want {
				t.Fatalf("CrossNamespace(%q,%q)=%v want %v", c.backendNamespace, c.routeNamespace, got, c.want)
			}
		})
	}
}

func TestPermitsSecret(t *testing.T) {
	// from a Gateway in ns-a to a Secret "cert" in ns-b.
	cases := []struct {
		name   string
		grants []gatewayv1beta1.ReferenceGrant
		want   bool
	}{
		{"no grants", nil, false},
		{
			"matching grant (Gateway->Secret, named)",
			[]gatewayv1beta1.ReferenceGrant{grant("ns-b",
				[]gatewayv1.ReferenceGrantFrom{from("gateway.networking.k8s.io", "Gateway", "ns-a")},
				[]gatewayv1.ReferenceGrantTo{to("", "Secret", strp("cert"))})},
			true,
		},
		{
			"matching grant (all Secrets)",
			[]gatewayv1beta1.ReferenceGrant{grant("ns-b",
				[]gatewayv1.ReferenceGrantFrom{from("gateway.networking.k8s.io", "Gateway", "ns-a")},
				[]gatewayv1.ReferenceGrantTo{to("", "Secret", nil)})},
			true,
		},
		{
			"wrong from kind (Service, not Gateway)",
			[]gatewayv1beta1.ReferenceGrant{grant("ns-b",
				[]gatewayv1.ReferenceGrantFrom{from("gateway.networking.k8s.io", "HTTPRoute", "ns-a")},
				[]gatewayv1.ReferenceGrantTo{to("", "Secret", nil)})},
			false,
		},
		{
			"wrong to kind (Service, not Secret)",
			[]gatewayv1beta1.ReferenceGrant{grant("ns-b",
				[]gatewayv1.ReferenceGrantFrom{from("gateway.networking.k8s.io", "Gateway", "ns-a")},
				[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)})},
			false,
		},
		{
			"grant in the wrong namespace",
			[]gatewayv1beta1.ReferenceGrant{grant("ns-a",
				[]gatewayv1.ReferenceGrantFrom{from("gateway.networking.k8s.io", "Gateway", "ns-a")},
				[]gatewayv1.ReferenceGrantTo{to("", "Secret", nil)})},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PermitsSecret(c.grants, "ns-a", "ns-b", "cert"); got != c.want {
				t.Fatalf("PermitsSecret=%v want %v", got, c.want)
			}
		})
	}
}

func TestPermitsBackend(t *testing.T) {
	const grp = gatewayv1.GroupName // gateway.networking.k8s.io

	cases := []struct {
		name   string
		grants []gatewayv1beta1.ReferenceGrant
		want   bool
	}{
		{
			name:   "no grants",
			grants: nil,
			want:   false,
		},
		{
			name: "matching grant, to with no name (all Services)",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
				),
			},
			want: true,
		},
		{
			name: "matching grant, to scoped to the exact Service name",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", strp("backend"))},
				),
			},
			want: true,
		},
		{
			name: "to scoped to a DIFFERENT Service name",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", strp("other"))},
				),
			},
			want: false,
		},
		{
			name: "grant in the WRONG namespace (not the backend's)",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-c",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
				),
			},
			want: false,
		},
		{
			name: "from with the WRONG route kind",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "TCPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
				),
			},
			want: false,
		},
		{
			name: "from with the WRONG route namespace",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-x")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
				),
			},
			want: false,
		},
		{
			name: "from with the WRONG group",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from("example.com", "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
				),
			},
			want: false,
		},
		{
			name: "to with a non-core group is not a Service target",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("example.com", "Service", nil)},
				),
			},
			want: false,
		},
		{
			name: "to with a non-Service kind",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{from(string(grp), "HTTPRoute", "ns-a")},
					[]gatewayv1.ReferenceGrantTo{to("", "Secret", nil)},
				),
			},
			want: false,
		},
		{
			name: "one of several from/to entries matches",
			grants: []gatewayv1beta1.ReferenceGrant{
				grant(
					"ns-b",
					[]gatewayv1.ReferenceGrantFrom{
						from(string(grp), "TCPRoute", "ns-a"),
						from(string(grp), "HTTPRoute", "ns-a"),
					},
					[]gatewayv1.ReferenceGrantTo{
						to("", "Service", strp("nope")),
						to("", "Service", strp("backend")),
					},
				),
			},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PermitsBackend(c.grants, string(grp), "HTTPRoute", "ns-a", "ns-b", "backend")
			if got != c.want {
				t.Fatalf("PermitsBackend=%v want %v", got, c.want)
			}
		})
	}
}
