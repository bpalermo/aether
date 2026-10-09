package rendertest

import (
	"strings"
	"testing"
)

const render = `---
# Source: x/templates/sa.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: probe
---
# Source: x/templates/empty.yaml
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: probe
  annotations:
    a: "1"
spec:
  template:
    metadata:
      labels:
        aether.io/managed: "true"
    spec:
      containers:
        - name: probe
      volumes:
        - name: sock
          csi: {driver: csi.aether.io}
        - name: tmp
          emptyDir: {}
---
apiVersion: example.io/v1
kind: Other
metadata:
  name: other
spec:
  template: not-a-pod-template
---
apiVersion: example.io/v1
kind: Other
metadata:
  name: mapping
spec:
  template:
    metadata:
      labels:
        aether.io/managed: "true"
    spec:
      size: 3
---
apiVersion: example.io/v1
kind: Other
metadata:
  name: empty
spec:
  template: {}
`

func TestParse(t *testing.T) {
	objects, err := Parse(render)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range objects {
		ids = append(ids, o.ID())
	}
	if got, want := strings.Join(ids, " "), "ServiceAccount/probe DaemonSet/probe Other/other Other/mapping Other/empty"; got != want {
		t.Fatalf("objects = %s, want %s", got, want)
	}

	workloads := Workloads(objects)
	if len(workloads) != 1 || workloads[0].ID() != "DaemonSet/probe" {
		t.Fatalf("Workloads = %v, want the DaemonSet alone (a `template` that is a scalar, an empty mapping, or a mapping with no containers is not a pod template)", workloads)
	}
	ds := Find(t, objects, "DaemonSet/probe")
	if ds.Metadata.Annotations["a"] != "1" {
		t.Errorf("object annotations = %v", ds.Metadata.Annotations)
	}
	if ds.Template.Metadata.Labels["aether.io/managed"] != "true" {
		t.Errorf("pod labels = %v", ds.Template.Metadata.Labels)
	}
	volumes := ds.Template.Spec.Volumes
	if len(volumes) != 2 || volumes[0].CSI == nil || volumes[0].CSI.Driver != "csi.aether.io" || volumes[1].CSI != nil {
		t.Errorf("volumes = %+v", volumes)
	}
}

func TestParseRejectsAnEmptyRender(t *testing.T) {
	if _, err := Parse("---\n# nothing\n"); err == nil {
		t.Fatal("a render with no object parsed without an error")
	}
}

// A document that does not parse is reported without its text: a render can
// hold a generated private key.
func TestParseWithholdsWhatItCouldNotRead(t *testing.T) {
	_, err := Parse("kind: Secret\ndata:\n  tls.key: [SENTINEL\n")
	if err == nil {
		t.Fatal("malformed YAML parsed without an error")
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("the error quotes the document: %v", err)
	}
}
