{{/*
Chart name, optionally overridden via nameOverride.
*/}}
{{- define "prober.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified, release-prefixed name.
*/}}
{{- define "prober.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Namespace the chart deploys into. Defaults to the release namespace.
*/}}
{{- define "prober.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "prober.serviceAccountName" -}}
{{- include "prober.fullname" . -}}
{{- end -}}

{{/*
Chart label value (name-version).
*/}}
{{- define "prober.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels (immutable subset).
*/}}
{{- define "prober.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prober.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: prober
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "prober.labels" -}}
helm.sh/chart: {{ include "prober.chart" . }}
{{ include "prober.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Upstream authorization list (config.aether.io/upstreams, proposal 004).

Unions the reachability tier's echo service names with the mesh_dns tier's
resolved authorities: a mesh_dns target dials a real ClusterIP, so its upstream
cluster must be authorized too. Each mesh_dns FQDN is reduced to its host (the
":port" is dropped) since authorization keys on the authority, not the port.
Empty when neither tier is configured.
*/}}
{{- define "prober.upstreams" -}}
{{- $u := list -}}
{{- range .Values.probe.reachabilityTargets -}}
{{- $u = append $u . -}}
{{- end -}}
{{- range .Values.probe.meshDNSTargets -}}
{{- $u = append $u (splitList ":" . | first) -}}
{{- end -}}
{{- join "," $u -}}
{{- end -}}
