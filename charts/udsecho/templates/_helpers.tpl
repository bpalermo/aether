{{/*
Chart label value (name-version).
*/}}
{{- define "udsecho.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Labels, in three sets (#1402; the same split as the aether chart's, #1363, and
the prober chart's, #1372):

  udsecho.selectorLabels  `app: <workload>`. Immutable: it is the Deployment's
                          selector (and the echo workloads' spread selector).
                          Takes the workload's name.
  udsecho.podLabels       the selector label + `aether.io/managed: "true"`, the
                          label that puts the pod in the mesh. What the POD
                          TEMPLATE carries, and all it carries: nothing here
                          may change from one release to the next, because a
                          pod template that changes rolls the pods. Takes the
                          workload's name.
  udsecho.labels          the standard labels, helm.sh/chart and
                          app.kubernetes.io/version among them. What an OBJECT
                          carries on its own metadata (the Deployments, the
                          ServiceAccounts, the EndpointPolicy). Takes
                          (dict "root" $ "workload" <name>).

Unlike the other two charts, the object labels do not include the pod labels:
`app` is this chart's selector key and `aether.io/managed` is an instruction to
the mesh about a pod, and neither says anything about a ServiceAccount.

Until chart 2.0.3 no object carried any label. The selectors and the pod
templates render byte for byte what they rendered before the helpers existed
(//charts/udsecho:udsecho_selectors_unchanged_test), and
//charts/udsecho:udsecho_pod_template_version_test renders the chart at two
versions and fails if a pod template differs.
*/}}
{{- define "udsecho.selectorLabels" -}}
app: {{ . }}
{{- end -}}
{{- define "udsecho.podLabels" -}}
{{ include "udsecho.selectorLabels" . }}
aether.io/managed: "true"
{{- end -}}
{{- define "udsecho.labels" -}}
helm.sh/chart: {{ include "udsecho.chart" .root }}
app.kubernetes.io/name: {{ .root.Chart.Name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .workload }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- with .root.Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
