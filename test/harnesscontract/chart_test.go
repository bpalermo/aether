package harnesscontract

import (
	"slices"
	"strings"
	"testing"
)

// render is a small `helm template` output: a comment-only document, a
// DaemonSet, a Deployment whose strategy is a percentage, and a Secret whose
// data must never appear in a failure.
const render = `---
# Source: x/templates/notes.yaml
---
# Source: x/templates/ds.yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: aether-agent
  namespace: aether-system
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: aether-agent
    spec:
      initContainers:
        - name: cni-install
      containers:
        - name: agent
          command: ["/agent"]
          args:
            - "--mesh-domain=aether.internal"
            - "--token=hunter2"
          env:
            - name: OTEL_RESOURCE_ATTRIBUTES
              value: "k8s.node.name=$(NODE_NAME),service.namespace=hunter2"
            - name: TOKEN
              value: hunter2
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: aether-edge
  namespace: aether-ingress
spec:
  strategy:
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 0
  template:
    metadata:
      labels: {app: edge}
    spec:
      containers:
        - name: envoy
---
apiVersion: v1
kind: Secret
metadata:
  name: webhook
data:
  tls.key: c2VjcmV0LWtleQ==
`

func agentObject() Object {
	return Object{
		ID: "o", Kind: "DaemonSet", Name: "aether-agent", Namespace: "aether-system",
		PodLabels:     map[string]string{"app.kubernetes.io/name": "aether-agent"},
		Containers:    []Container{{Name: "agent", EnvContains: map[string][]string{"OTEL_RESOURCE_ATTRIBUTES": {"k8s.node.name="}}}},
		RollingUpdate: &RollingUpdate{MaxSurge: "0", MaxUnavailable: "1"},
	}
}

func TestRenderCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Object)
		want string // "" = no problem
	}{
		"as rendered":          {func(*Object) {}, ""},
		"a renamed workload":   {func(o *Object) { o.Name = "aether-node-agent" }, "DaemonSet/aether-node-agent is not rendered (the render's DaemonSet objects: aether-agent)"},
		"another kind":         {func(o *Object) { o.Kind = "StatefulSet" }, "is not rendered (the render's StatefulSet objects: none)"},
		"another namespace":    {func(o *Object) { o.Namespace = "mesh" }, `is in namespace "aether-system", the contract says "mesh"`},
		"a renamed pod label":  {func(o *Object) { o.PodLabels = map[string]string{"app": "agent"} }, "has no pod label app=agent (its pod labels: app.kubernetes.io/name=aether-agent)"},
		"a renamed container":  {func(o *Object) { o.Containers[0].Name = "node-agent" }, `has no container "node-agent" (its containers: agent)`},
		"an init container":    {func(o *Object) { o.Containers[0] = Container{Name: "cni-install"} }, `has no container "cni-install"`},
		"another strategy":     {func(o *Object) { o.RollingUpdate.MaxSurge = "1" }, "rolls with maxSurge=0 maxUnavailable=1, the contract says maxSurge=1 maxUnavailable=1"},
		"an env without value": {func(o *Object) { o.Containers[0].EnvContains["OTEL_RESOURCE_ATTRIBUTES"] = []string{"k8s.pod.name="} }, `OTEL_RESOURCE_ATTRIBUTES does not contain "k8s.pod.name="`},
		"an env that is unset": {func(o *Object) { o.Containers[0].EnvContains = map[string][]string{"NODE": {"x"}} }, `NODE does not contain "x"`},
		"a percentage": {func(o *Object) {
			*o = Object{ID: "e", Kind: "Deployment", Name: "aether-edge", RollingUpdate: &RollingUpdate{MaxSurge: "25%", MaxUnavailable: "0"}}
		}, ""},
		"no strategy rendered": {func(o *Object) {
			*o = Object{ID: "s", Kind: "Secret", Name: "webhook", RollingUpdate: &RollingUpdate{MaxSurge: "1"}}
		}, "rolls with maxSurge=unset maxUnavailable=unset"},
	} {
		t.Run(name, func(t *testing.T) {
			o := agentObject()
			tc.edit(&o)
			got := strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(render)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
			// Whatever is reported, no value of the render that could be a
			// secret is in it.
			for _, secret := range []string{"hunter2", "c2VjcmV0LWtleQ=="} {
				if strings.Contains(got, secret) {
					t.Errorf("Check() printed %q from the render: %q", secret, got)
				}
			}
		})
	}
}

// linked is a contract whose one container refers to a resource attribute and
// takes an argument from a name, loaded as the real one is.
const linked = `
version: 1
resource_attributes:
  - {id: resource.node, attribute: ATTRIBUTE, components: [agent], checked_by: review-only}
names:
  - {id: mesh.default_domain, value: DOMAIN, checked_by: review-only}
charts:
  - id: r
    chart: x
    release: x
    namespace: n
    objects:
      - id: o
        kind: DaemonSet
        name: aether-agent
        containers:
          - name: agent
            resource_attributes: [resource.node]
            args: {FLAG: mesh.default_domain}
    checked_by: review-only
`

// TestRenderCheck_LinkedEntries: a container's resource attribute and argument
// are read from the entries it refers to, so changing either entry alone, or
// the chart alone, is a difference.
func TestRenderCheck_LinkedEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		attribute, value, flag, domain string
		render                         string
		want                           string
	}{
		"as rendered": {attribute: "k8s.node.name", flag: "--mesh-domain", domain: "aether.internal"},
		"the contract's attribute is edited alone": {
			attribute: "k8s.node", flag: "--mesh-domain", domain: "aether.internal",
			want: `OTEL_RESOURCE_ATTRIBUTES sets no resource attribute "k8s.node", which the entry resource.node says the component carries (it sets: k8s.node.name, service.namespace)`,
		},
		"an attribute that is only the end of a rendered key": {
			attribute: "node.name", flag: "--mesh-domain", domain: "aether.internal",
			want: `sets no resource attribute "node.name"`,
		},
		"the chart stops passing the variable": {
			attribute: "k8s.node.name", flag: "--mesh-domain", domain: "aether.internal",
			render: strings.Replace(render, "name: OTEL_RESOURCE_ATTRIBUTES", "name: OTEL_ATTRIBUTES", 1),
			want:   `sets no resource attribute "k8s.node.name"`,
		},
		"an attribute with a value the render does not give it": {
			attribute: "k8s.node.name", value: "worker", flag: "--mesh-domain", domain: "aether.internal",
			want: `sets k8s.node.name to something other than "worker", the value of the entry resource.node`,
		},
		"the chart's default domain is edited alone": {
			attribute: "k8s.node.name", flag: "--mesh-domain", domain: "aether.internal",
			render: strings.Replace(render, "--mesh-domain=aether.internal", "--mesh-domain=mesh.internal", 1),
			want:   `is not run with --mesh-domain=aether.internal, the value of the entry mesh.default_domain (it has --mesh-domain with another value)`,
		},
		"the contract's domain is edited alone": {
			attribute: "k8s.node.name", flag: "--mesh-domain", domain: "mesh.internal",
			want: `is not run with --mesh-domain=mesh.internal`,
		},
		"a domain that is only the start of the rendered one": {
			attribute: "k8s.node.name", flag: "--mesh-domain", domain: "aether",
			want: `it has --mesh-domain with another value`,
		},
		"a flag the container is not run with": {
			attribute: "k8s.node.name", flag: "--domain", domain: "aether.internal",
			want: `is not run with --domain=aether.internal, the value of the entry mesh.default_domain (it has no --domain argument)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.NewReplacer("ATTRIBUTE", tc.attribute, "FLAG", tc.flag, "DOMAIN", tc.domain).Replace(linked)
			if tc.value != "" {
				doc = strings.Replace(doc, "attribute: "+tc.attribute, "attribute: "+tc.attribute+", value: "+tc.value, 1)
			}
			c, err := parse([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			from := render
			if tc.render != "" {
				from = tc.render
			}
			got := strings.Join(c.Charts[0].Check([]byte(from)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("Check() printed a value of the render: %q", got)
			}
		})
	}
}

// tied is a contract whose one object is named by a `names` entry, and whose
// container takes an argument from a pattern and must not be given a resource
// attribute its own code sets.
const tied = `
version: 1
resource_attributes:
  - {id: sn, attribute: service.name, value: svc, components: [agent], checked_by: review-only}
names:
  - {id: port, value: "PORT", checked_by: review-only}
  - {id: ds, value: NAME, checked_by: review-only}
charts:
  - id: r
    chart: x
    release: x
    namespace: n
    objects:
      - id: o
        kind: DaemonSet
        name_from: ds
        containers:
          - name: agent
            code_resource_attributes: [sn]
            args: {--egress: "127.0.0.1:<port>"}
    checked_by: review-only
`

// TestRenderCheck_Ties: an object's name, an argument made of a pattern and a
// resource attribute the code sets are each read from the entry, so changing
// the entry alone, or the chart alone, is a difference.
func TestRenderCheck_Ties(t *testing.T) {
	// The render, with the argument the pattern describes.
	rendered := strings.Replace(render, `- "--token=hunter2"`, `- "--token=hunter2"`+"\n            - \"--egress=127.0.0.1:18081\"", 1)
	env := func(variable string) string {
		return strings.Replace(rendered, "            - name: TOKEN\n", "            - name: "+variable+"\n            - name: TOKEN\n", 1)
	}
	for name, tc := range map[string]struct {
		name, port string
		render     string
		want       string
	}{
		"as rendered": {name: "aether-agent", port: "18081"},
		"the name's entry is edited alone": {
			name: "aether-node-agent", port: "18081",
			want: "o: DaemonSet/aether-node-agent (named by the entry ds) is not rendered (the render's DaemonSet objects: aether-agent)",
		},
		"the chart renames the object alone": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, "  name: aether-agent\n", "  name: aether-node-agent\n", 1),
			want:   "o: DaemonSet/aether-agent (named by the entry ds) is not rendered (the render's DaemonSet objects: aether-node-agent)",
		},
		"the port's entry is edited alone": {
			name: "aether-agent", port: "18091",
			want: `is not run with --egress=127.0.0.1:18091, which is what the contract's 127.0.0.1:<port> comes to (it has --egress with another value)`,
		},
		"the chart's default port is edited alone": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, "--egress=127.0.0.1:18081", "--egress=127.0.0.1:19000", 1),
			want:   `is not run with --egress=127.0.0.1:18081`,
		},
		"the chart's default is another host": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, "--egress=127.0.0.1:18081", "--egress=localhost:18081", 1),
			want:   `is not run with --egress=127.0.0.1:18081`,
		},
		"the chart stops passing the argument": {
			name: "aether-agent", port: "18081",
			render: render,
			want:   `(it has no --egress argument)`,
		},
		"the chart gives the container the attribute its code sets": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, "service.namespace=hunter2", "service.namespace=hunter2,service.name=hunter2", 1),
			want:   `OTEL_RESOURCE_ATTRIBUTES sets the resource attribute "service.name", which the entry sn says the component's own code sets`,
		},
		"the chart gives the container OTEL_SERVICE_NAME": {
			name: "aether-agent", port: "18081",
			render: env("OTEL_SERVICE_NAME\n              value: hunter2"),
			want:   `container "agent" is given OTEL_SERVICE_NAME, which replaces the service.name the entry sn says the component's own code sets`,
		},
		"the chart gives it OTEL_SERVICE_NAME from a field": {
			name: "aether-agent", port: "18081",
			render: env("OTEL_SERVICE_NAME\n              valueFrom: {fieldRef: {fieldPath: metadata.name}}"),
			want:   `is given OTEL_SERVICE_NAME`,
		},
		// What the check cannot read could set the attribute: it fails closed.
		"the chart takes the resource attributes from a source that cannot be read": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, `value: "k8s.node.name=$(NODE_NAME),service.namespace=hunter2"`, "valueFrom: {configMapKeyRef: {name: hunter2, key: attrs}}", 1),
			want:   "takes OTEL_RESOURCE_ATTRIBUTES from `valueFrom`, which this check cannot read",
		},
		"the chart gives the container variables in bulk": {
			name: "aether-agent", port: "18081",
			render: strings.Replace(rendered, "          env:\n", "          envFrom:\n            - configMapRef: {name: hunter2}\n          env:\n", 1),
			want:   "takes variables from `envFrom`, which this check cannot read",
		},
		// service.namespace is in the render all along: only the attribute
		// itself counts, and a variable of another name is not the SDK's.
		"another variable": {name: "aether-agent", port: "18081", render: env("OTEL_SERVICE_NAMES\n              value: hunter2")},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := parse([]byte(strings.NewReplacer("NAME", tc.name, "PORT", tc.port).Replace(tied)))
			if err != nil {
				t.Fatal(err)
			}
			from := rendered
			if tc.render != "" {
				from = tc.render
			}
			got := strings.Join(c.Charts[0].Check([]byte(from)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("Check() printed a value of the render: %q", got)
			}
		})
	}
}

// A container or an object built by hand refers to entries nothing resolved:
// that is a difference, never a pass.
func TestRenderCheck_UnresolvedReferences(t *testing.T) {
	o := agentObject()
	o.Containers[0].ResourceAttributes = []string{"resource.node"}
	got := strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(render)), "\n")
	if !strings.Contains(got, "refers to other entries (resource.node) and they were not resolved") {
		t.Errorf("Check() = %q", got)
	}
	o = agentObject()
	o.Containers[0].CodeResourceAttributes = []string{"sn"}
	got = strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(render)), "\n")
	if !strings.Contains(got, "refers to other entries (sn) and they were not resolved") {
		t.Errorf("Check() = %q", got)
	}
	o = agentObject()
	o.Name, o.NameFrom = "", "ds"
	got = strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(render)), "\n")
	if !strings.Contains(got, "o refers to other entries (ds) and they were not resolved") {
		t.Errorf("Check() = %q", got)
	}
}

// selected is a contract that holds a pod's hostPath volume and the selectors
// of two webhooks to `names` entries.
const selected = `
version: 1
names:
  - {id: driver, value: DRIVER, checked_by: review-only}
  - {id: label, value: LABEL, checked_by: review-only}
charts:
  - id: r
    chart: x
    release: x
    namespace: ns
    objects:
      - id: o
        kind: DaemonSet
        name: plugin
        host_paths: ["/plugins/<driver>"]
        containers: [{name: plugin}]
      - id: w
        kind: MutatingWebhookConfiguration
        webhooks:
          inject.example: {namespaces: label}
          ndots.example: {objects: label}
    checked_by: review-only
`

const selectedRender = `---
kind: DaemonSet
metadata: {name: plugin}
spec:
  template:
    spec:
      initContainers:
        - name: prepare
          volumeMounts:
            - {name: registry, mountPath: /registry}
      containers:
        - name: plugin
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}
            - {name: registry, mountPath: /var/lib/kubelet/plugins_registry}
        - name: sidecar
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: tmp
          emptyDir: {}
        - name: plugin-dir
          hostPath: {path: /var/lib/kubelet/plugins/csi.example.io}
        - name: registry
          hostPath: {path: /var/lib/kubelet/plugins_registry}
---
kind: MutatingWebhookConfiguration
metadata: {name: whatever-the-release-makes-it}
webhooks:
  - name: inject.example
    namespaceSelector: {matchLabels: {example.io/managed: "true"}}
  - name: ndots.example
    objectSelector: {matchLabels: {example.io/managed: "true"}}
  - name: other.example
---
kind: ValidatingWebhookConfiguration
metadata: {name: validate}
webhooks:
  - name: inject.example
`

// TestRenderCheck_HostPathsAndWebhooks: the directory a pod mounts from the
// host and the label a webhook selects by are read from the entries, so the
// entry edited alone, or the chart alone, is a difference.
func TestRenderCheck_HostPathsAndWebhooks(t *testing.T) {
	for name, tc := range map[string]struct {
		driver, label string
		render        string
		want          string
	}{
		"as rendered": {driver: "csi.example.io", label: "example.io/managed"},
		"the driver's entry is edited alone": {
			driver: "csi.mesh.io", label: "example.io/managed",
			want: "o: DaemonSet/plugin has no hostPath volume whose path ends with /plugins/csi.mesh.io, which is what the contract's /plugins/<driver> comes to (its hostPath volumes: /var/lib/kubelet/plugins/csi.example.io, /var/lib/kubelet/plugins_registry)",
		},
		"the chart's directory is edited alone": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "hostPath: {path: /var/lib/kubelet/plugins/csi.example.io}", "hostPath: {path: /var/lib/kubelet/plugins/csi.mesh.io}", 1),
			want:   "has no hostPath volume whose path ends with /plugins/csi.example.io",
		},
		"a driver that is only the end of the directory's name": {
			driver: "example.io", label: "example.io/managed",
			want: "has no hostPath volume whose path ends with /plugins/example.io",
		},
		"the label's entry is edited alone": {
			driver: "csi.example.io", label: "example.io/meshed",
			want: `w: the render's one MutatingWebhookConfiguration: the namespaceSelector of the webhook "inject.example" does not select by the label example.io/meshed=true, the value of the entry label with "true" (it selects by: example.io/managed=true)`,
		},
		"the chart's namespace selector is edited alone": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "namespaceSelector: {matchLabels: {example.io/managed:", "namespaceSelector: {matchLabels: {example.io/meshed:", 1),
			want:   `the namespaceSelector of the webhook "inject.example" does not select by the label example.io/managed=true`,
		},
		"the chart's object selector is edited alone": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "objectSelector: {matchLabels: {example.io/managed:", "objectSelector: {matchLabels: {example.io/meshed:", 1),
			want:   `the objectSelector of the webhook "ndots.example" does not select by the label example.io/managed=true, the value of the entry label with "true" (it selects by: example.io/meshed=true)`,
		},
		// The two selectors are not interchangeable: the label moved from one
		// to the other selects other things.
		"the chart moves the label to the object selector": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "namespaceSelector: {matchLabels: {example.io/managed:", "objectSelector: {matchLabels: {example.io/managed:", 1),
			want:   `the namespaceSelector of the webhook "inject.example" does not select by the label example.io/managed=true, the value of the entry label with "true" (it selects by: none)`,
		},
		"the chart moves the label to the namespace selector": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "    objectSelector: {matchLabels: {example.io/managed:", "    namespaceSelector: {matchLabels: {example.io/managed:", 1),
			want:   `the objectSelector of the webhook "ndots.example" does not select by the label example.io/managed=true`,
		},
		// The key with another value matches no pod the mesh manages.
		"the chart selects the label with another value": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `objectSelector: {matchLabels: {example.io/managed: "true"}}`, `objectSelector: {matchLabels: {example.io/managed: "false"}}`, 1),
			want:   `the objectSelector of the webhook "ndots.example" does not select by the label example.io/managed=true, the value of the entry label with "true" (it selects by: example.io/managed=false)`,
		},
		// The plugin writes its socket under the directory named after the
		// driver, in its own filesystem: the kubelet sees it only if the host's
		// directory is mounted there.
		"the chart no longer mounts the directory": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}\n", "", 1),
			want:   `o: DaemonSet/plugin: no container mounts the hostPath volume "plugin-dir" (/var/lib/kubelet/plugins/csi.example.io) at a path ending with /plugins/csi.example.io, which is what the contract's /plugins/<driver> comes to (the containers the contract lists for the object, plugin, mount it at: nowhere)`,
		},
		"the chart mounts the directory elsewhere": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "{name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}", "{name: plugin-dir, mountPath: /csi}", 1),
			want:   `no container mounts the hostPath volume "plugin-dir" (/var/lib/kubelet/plugins/csi.example.io) at a path ending with /plugins/csi.example.io, which is what the contract's /plugins/<driver> comes to (the containers the contract lists for the object, plugin, mount it at: /csi)`,
		},
		"another volume is mounted at the directory": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "{name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}", "{name: tmp, mountPath: /var/lib/kubelet/plugins/csi.example.io}", 1),
			want:   `no container mounts the hostPath volume "plugin-dir"`,
		},
		"only an init container mounts the directory": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(strings.Replace(selectedRender, "            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}\n", "", 1),
				"{name: registry, mountPath: /registry}", "{name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}", 1),
			want: `no container mounts the hostPath volume "plugin-dir"`,
		},
		// The process that writes the socket is the one the contract lists.
		"only a sidecar mounts the directory": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(strings.Replace(selectedRender, "            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}\n", "", 1),
				"        - name: sidecar\n          volumeMounts:\n", "        - name: sidecar\n          volumeMounts:\n            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}\n", 1),
			want: `no container mounts the hostPath volume "plugin-dir" (/var/lib/kubelet/plugins/csi.example.io) at a path ending with /plugins/csi.example.io, which is what the contract's /plugins/<driver> comes to (the containers the contract lists for the object, plugin, mount it at: nowhere)`,
		},
		// A webhook's two selectors are ANDed as well: the one the contract
		// does not name must select nothing in particular.
		"the chart adds an object selector to the webhook held by its namespaces": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `    namespaceSelector: {matchLabels: {example.io/managed: "true"}}`, `    namespaceSelector: {matchLabels: {example.io/managed: "true"}}`+"\n"+`    objectSelector: {matchLabels: {tier: mesh}}`, 1),
			want:   `w: the render's one MutatingWebhookConfiguration: the webhook "inject.example" also selects with its objectSelector (tier=mesh), and the contract holds it to its other selector alone`,
		},
		"the chart adds a namespace selector to the webhook held by its objects": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `    objectSelector: {matchLabels: {example.io/managed: "true"}}`, `    objectSelector: {matchLabels: {example.io/managed: "true"}}`+"\n"+`    namespaceSelector: {matchExpressions: [{key: tier, operator: Exists}]}`, 1),
			want:   `the webhook "ndots.example" also selects with its namespaceSelector (none, and 1 matchExpressions), and the contract holds it to its other selector alone`,
		},
		// Kubernetes ANDs the requirements of a selector: anything beside the
		// one pair narrows what the webhook sees, and it ignores failures.
		"the chart requires one more label of a namespace": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `namespaceSelector: {matchLabels: {example.io/managed: "true"}}`, `namespaceSelector: {matchLabels: {example.io/managed: "true", tier: mesh}}`, 1),
			want:   `w: the render's one MutatingWebhookConfiguration: the namespaceSelector of the webhook "inject.example" selects by more than the label example.io/managed=true, the value of the entry label with "true" (it selects by: example.io/managed=true, tier=mesh)`,
		},
		"the chart adds an expression to a selector": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `objectSelector: {matchLabels: {example.io/managed: "true"}}`, `objectSelector: {matchLabels: {example.io/managed: "true"}, matchExpressions: [{key: example.io/managed, operator: NotIn, values: ["true"]}]}`, 1),
			want:   `the objectSelector of the webhook "ndots.example" selects by more than the label example.io/managed=true, the value of the entry label with "true" (it selects by: example.io/managed=true, and 1 matchExpressions)`,
		},
		"the chart selects by an expression alone": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, `objectSelector: {matchLabels: {example.io/managed: "true"}}`, `objectSelector: {matchExpressions: [{key: example.io/managed, operator: In, values: ["true"]}]}`, 1),
			want:   `the objectSelector of the webhook "ndots.example" does not select by the label example.io/managed=true, the value of the entry label with "true" (it selects by: none, and 1 matchExpressions)`,
		},
		"a webhook that selects by nothing": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "    objectSelector: {matchLabels: {example.io/managed: \"true\"}}\n", "", 1),
			want:   `the objectSelector of the webhook "ndots.example" does not select by the label example.io/managed=true, the value of the entry label with "true" (it selects by: none)`,
		},
		"a webhook the chart no longer renders": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "  - name: ndots.example\n", "  - name: dots.example\n", 1),
			want:   `has no webhook "ndots.example" (its webhooks: inject.example, dots.example, other.example)`,
		},
		"a second configuration of the kind": {
			driver: "csi.example.io", label: "example.io/managed",
			render: selectedRender + "---\nkind: MutatingWebhookConfiguration\nmetadata: {name: second}\n",
			want:   "w: the render's one MutatingWebhookConfiguration is rendered 2 times",
		},
		"no configuration of the kind": {
			driver: "csi.example.io", label: "example.io/managed",
			render: strings.Replace(selectedRender, "kind: MutatingWebhookConfiguration", "kind: Something", 1),
			want:   "w: the render's one MutatingWebhookConfiguration is not rendered",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := parse([]byte(strings.NewReplacer("DRIVER", tc.driver, "LABEL", tc.label).Replace(selected)))
			if err != nil {
				t.Fatal(err)
			}
			from := selectedRender
			if tc.render != "" {
				from = tc.render
			}
			got := strings.Join(c.Charts[0].Check([]byte(from)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
	// Built by hand, nothing resolved the entries: never a pass.
	for _, o := range []Object{
		{ID: "o", Kind: "DaemonSet", Name: "plugin", HostPaths: []string{"/plugins/<driver>"}},
		{ID: "o", Kind: "MutatingWebhookConfiguration", Webhooks: map[string]WebhookSelector{"inject.example": {Objects: "driver"}}},
	} {
		got := strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(selectedRender)), "\n")
		if !strings.Contains(got, "o refers to other entries (driver) and they were not resolved") {
			t.Errorf("Check() = %q", got)
		}
	}
}

// rooted is a contract that holds a whole host path: the root a container is
// run with, then a directory named after an entry.
const rooted = `
version: 1
names:
  - {id: driver, value: csi.example.io, checked_by: review-only}
charts:
  - id: r
    chart: x
    release: x
    namespace: ns
    objects:
      - id: o
        kind: DaemonSet
        name: plugin
        host_paths: ["<arg:--kubelet-root>/plugins/<driver>"]
        containers: [{name: plugin}]
    checked_by: review-only
`

const rootedRender = `---
kind: DaemonSet
metadata: {name: plugin}
spec:
  template:
    spec:
      containers:
        - name: plugin
          args: ["--kubelet-root=/var/lib/kubelet", "--root=/run/x"]
          volumeMounts:
            - {name: plugin-dir, mountPath: /var/lib/kubelet/plugins/csi.example.io}
        - name: sidecar
          args: ["--kubelet-root=/wrong"]
      volumes:
        - name: plugin-dir
          hostPath: {path: /var/lib/kubelet/plugins/csi.example.io}
        - name: registry
          hostPath: {path: /var/lib/kubelet/plugins_registry}
`

// TestRenderCheck_RootedHostPath: the directory is held whole. The kubelet
// looks under its own root, which the plugin is told with a flag: a directory
// with the right end under another root is one the kubelet never reads.
func TestRenderCheck_RootedHostPath(t *testing.T) {
	c, err := parse([]byte(rooted))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Charts[0].refs(); !slices.Equal(got, []string{"driver"}) {
		t.Fatalf("the render refers to %q, want the driver alone: the flag is not an entry", got)
	}
	for name, tc := range map[string]struct {
		from, to string
		all      bool
		want     string
	}{
		"as rendered": {},
		"the chart moves the volume and the mount to another root": {
			from: "/var/lib/kubelet/plugins/csi.example.io", to: "/wrong/plugins/csi.example.io", all: true,
			want: `o: DaemonSet/plugin has no hostPath volume of the path /var/lib/kubelet/plugins/csi.example.io, which is what the contract's <arg:--kubelet-root>/plugins/<driver> comes to with the --kubelet-root of container "plugin" (its hostPath volumes: /wrong/plugins/csi.example.io, /var/lib/kubelet/plugins_registry)`,
		},
		"the chart gives the plugin another root and leaves the directory": {
			from: `"--kubelet-root=/var/lib/kubelet"`, to: `"--kubelet-root=/var/lib/k0s/kubelet"`,
			want: `has no hostPath volume of the path /var/lib/k0s/kubelet/plugins/csi.example.io`,
		},
		"the chart mounts the directory under another root": {
			from: "mountPath: /var/lib/kubelet/plugins/csi.example.io", to: "mountPath: /wrong/plugins/csi.example.io",
			want: `o: DaemonSet/plugin container "plugin" does not mount the hostPath volume "plugin-dir" at /var/lib/kubelet/plugins/csi.example.io, which is what the contract's <arg:--kubelet-root>/plugins/<driver> comes to with its --kubelet-root (it mounts it at: /wrong/plugins/csi.example.io)`,
		},
		"the chart names the directory after another driver": {
			from: "hostPath: {path: /var/lib/kubelet/plugins/csi.example.io}", to: "hostPath: {path: /var/lib/kubelet/plugins/csi.mesh.io}",
			want: `has no hostPath volume of the path /var/lib/kubelet/plugins/csi.example.io`,
		},
		"the plugin is no longer told the root": {
			from: `"--kubelet-root=/var/lib/kubelet", `, to: ``,
			want: `o: DaemonSet/plugin container "plugin" is not run with --kubelet-root, which the contract's <arg:--kubelet-root>/plugins/<driver> starts with`,
		},
		"the plugin container is gone": {
			from: "        - name: plugin\n", to: "        - name: csi\n",
			want: `o: DaemonSet/plugin has no container to take --kubelet-root from`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			from := rootedRender
			if tc.from != "" {
				if !strings.Contains(from, tc.from) {
					t.Fatalf("the render has no %q", tc.from)
				}
				n := 1
				if tc.all {
					n = -1
				}
				from = strings.Replace(from, tc.from, tc.to, n)
			}
			got := strings.Join(c.Charts[0].Check([]byte(from)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// byTest is a contract with a chart test per chart, and a name the first of
// them compares with its render.
const byTest = `
version: 1
names:
  - {id: nm, value: v, checked_by: [//a:b, //t:x]}
charts:
  - {id: rx, chart: x, release: r, namespace: ns, objects: [{id: rx.o, kind: K, name_from: nm}], checked_by: //t:x}
  - {id: ry, chart: y, release: r, namespace: ns, objects: [{id: ry.o, kind: K, name: z}], checked_by: //t:y}
`

// TestHeldByChartTest: a chart test holds the renders of its chart and what
// they refer to, and those entries name it; nothing else names it.
func TestHeldByChartTest(t *testing.T) {
	c, err := parse([]byte(byTest))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.HeldByChartTest("//t:x", "x", []string{"rx", "rx.o", "nm"}); len(got) != 0 {
		t.Errorf("HeldByChartTest() = %q, want nothing", got)
	}
	for name, tc := range map[string]struct {
		target, chart string
		ids           []string
		want          []string
	}{
		"an entry the test is named for and does not list": {"//t:x", "x", []string{"rx", "rx.o"}, []string{`holds this chart to the entry "nm"`}},
		"a render that names another test": {
			"//t:y", "x",
			[]string{"rx", "rx.o", "nm"},
			[]string{
				`rx is a render of the chart "x" and its checked_by does not name //t:y, the test that renders that chart`,
				`names //t:y in the checked_by of "ry", and that entry is neither a render of the chart "x" nor referred to by one`,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(c.HeldByChartTest(tc.target, tc.chart, tc.ids), "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("HeldByChartTest() = %q, want it to contain %q", got, want)
				}
			}
			if !strings.Contains(got, File) {
				t.Errorf("a failure does not name the contract file: %q", got)
			}
		})
	}
}

func TestRenderCheck_ObjectRenderedTwice(t *testing.T) {
	twice := render + "---\nkind: Secret\nmetadata:\n  name: webhook\n"
	got := Render{ID: "r", Objects: []Object{{ID: "s", Kind: "Secret", Name: "webhook"}}}.Check([]byte(twice))
	if len(got) != 1 || !strings.Contains(got[0], "is rendered 2 times") {
		t.Errorf("Check() = %q", got)
	}
}

// A document that is not YAML is reported without its text.
func TestRenderCheck_UnreadableDocument(t *testing.T) {
	got := Render{ID: "r", Objects: []Object{agentObject()}}.Check([]byte("kind: [secret-looking-text\n"))
	if len(got) != 1 || !strings.Contains(got[0], "not YAML this check can read") || strings.Contains(got[0], "secret-looking-text") {
		t.Errorf("Check() = %q", got)
	}
}

func TestOwnedIDs(t *testing.T) {
	renders := []Render{{ID: "r", Objects: []Object{{ID: "r.a"}, {ID: "r.b"}}}}
	if got := OwnedIDs(renders, []string{"r", "r.a", "r.b"}); len(got) != 0 {
		t.Errorf("OwnedIDs() = %q, want nothing", got)
	}
	// An entry of another section a container refers to is held by the test
	// too: its id is in the list, and stays there until the tie is removed.
	tied := []Render{{ID: "r", Objects: []Object{{ID: "r.a", Containers: []Container{{Name: "c", ResourceAttributes: []string{"ra"}, Args: map[string]string{"--f": "n"}}}}}}}
	if got := OwnedIDs(tied, []string{"r", "r.a", "ra", "n"}); len(got) != 0 {
		t.Errorf("OwnedIDs() = %q, want nothing", got)
	}
	untied := strings.Join(OwnedIDs(tied, []string{"r", "r.a"}), "\n")
	for _, want := range []string{`holds this chart to the entry "n"`, `holds this chart to the entry "ra"`} {
		if !strings.Contains(untied, want) {
			t.Errorf("OwnedIDs() = %q, want it to contain %q", untied, want)
		}
	}
	if got := strings.Join(OwnedIDs(renders, []string{"r", "r.a", "r.b", "ra"}), "\n"); !strings.Contains(got, `nothing in this chart's renders refers to an entry of that id any more`) {
		t.Errorf("OwnedIDs() = %q, want the tie that is gone", got)
	}
	got := strings.Join(OwnedIDs(renders, []string{"r", "r.a", "r.gone"}), "\n")
	for _, want := range []string{`no longer has the chart entry "r.gone"`, `has the chart entry "r.b", and the test that renders its chart does not list it`} {
		if !strings.Contains(got, want) {
			t.Errorf("OwnedIDs() = %q, want it to contain %q", got, want)
		}
	}
}

func TestHelmArgs(t *testing.T) {
	r := Render{Release: "aether", Namespace: "aether-system", Set: map[string]string{"b.c": "true", "a": "1"}}
	want := []string{"template", "aether", "chart.tgz", "--namespace", "aether-system", "--set", "a=1", "--set", "b.c=true"}
	if got := r.HelmArgs("chart.tgz"); !slices.Equal(got, want) {
		t.Errorf("HelmArgs() = %q, want %q", got, want)
	}
}

func TestRendersOf(t *testing.T) {
	c := MustLoad(t)
	if got := c.RendersOf("aether"); len(got) < 2 {
		t.Errorf("the contract has %d renders of the aether chart, want the default one and the surge one", len(got))
	}
	if got := c.RendersOf("no-such-chart"); len(got) != 0 {
		t.Errorf("RendersOf(no-such-chart) = %v", got)
	}
}
