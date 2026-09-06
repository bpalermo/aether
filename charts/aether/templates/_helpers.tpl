{{/*
Base release-prefixed name. Component resources derive from this so the agent,
registrar and controller objects get distinct names within one release.
*/}}
{{- define "aether.fullname" -}}
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

{{/* Namespace the chart deploys into. Defaults to the release namespace. */}}
{{- define "aether.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/* Chart label value (name-version). */}}
{{- define "aether.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Render an image reference from a {repository, tag, digest} dict so consumers can
mirror images to a private registry by overriding `repository` alone. A digest is
preferred (immutable pin); otherwise a tag; repository alone if neither is set.
Usage: {{ include "aether.image" .Values.agent.image }}
*/}}
{{- define "aether.image" -}}
{{- if .digest -}}
{{- printf "%s@%s" .repository .digest -}}
{{- else if .tag -}}
{{- printf "%s:%s" .repository .tag -}}
{{- else -}}
{{- .repository -}}
{{- end -}}
{{- end -}}

{{/*
Name of the MeshConfig ConfigMap the controller projects and the agent mounts.
Release-derived so both sides agree within the single release.
*/}}
{{- define "aether.meshConfigMapName" -}}
{{- printf "%s-mesh-config" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* ------------------------------------------------------------------ agent */}}
{{- define "aether.agent.fullname" -}}
{{- printf "%s-agent" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.agent.serviceAccountName" -}}{{ include "aether.agent.fullname" . }}{{- end -}}
{{- define "aether.agent.clusterScopedName" -}}
{{- printf "%s-%s" (include "aether.agent.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.agent.selectorLabels" -}}
app.kubernetes.io/name: aether-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end -}}
{{- define "aether.agent.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.agent.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* -------------------------------------------------------------- mesh-dns */}}
{{- define "aether.meshDns.fullname" -}}
{{- printf "%s-mesh-dns" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.meshDns.selectorLabels" -}}
app.kubernetes.io/name: aether-mesh-dns
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: mesh-dns
{{- end -}}
{{- define "aether.meshDns.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.meshDns.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
{{/*
aether.meshDns.lameDuckSeconds converts agent.meshDnsDaemon.lameDuckMax (a Go duration
string, e.g. "10s") to whole seconds, so terminationGracePeriodSeconds can be DERIVED
from it rather than hand-maintained alongside it (issue #729). A grace period shorter
than the lame-duck ceiling means the kubelet SIGKILLs mid-window — which is precisely
the abrupt socket close the window exists to avoid — so the two must never drift.
Accepts h/m/s/ms suffixes and a bare number (read as seconds); "ms" is rounded UP so a
sub-second window still gets a non-zero budget.

An UNSET (null) value renders no flag, so the binary falls back to its own
DefaultLameDuckMax — and the grace period must follow it there, not to zero, or the
kubelet would SIGKILL 5s into a 10s window. Keep this literal in step with
meshdns.DefaultLameDuckMax.
*/}}
{{- define "aether.meshDns.lameDuckSeconds" -}}
{{- $v := . | toString | trim | lower -}}
{{- if or (kindIs "invalid" .) (eq $v "") -}}
{{- 10 -}}
{{- else -}}
{{- $n := regexFind "^[0-9]+" $v | default "0" | atoi -}}
{{- if hasSuffix "ms" $v -}}
{{- div (add $n 999) 1000 -}}
{{- else if hasSuffix "h" $v -}}
{{- mul $n 3600 -}}
{{- else if hasSuffix "m" $v -}}
{{- mul $n 60 -}}
{{- else -}}
{{- $n -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------------------------ proxy */}}
{{- define "aether.proxy.fullname" -}}{{- "aether-proxy" -}}{{- end -}}
{{- define "aether.proxy.serviceAccountName" -}}{{ include "aether.proxy.fullname" . }}{{- end -}}
{{- define "aether.proxy.configMapName" -}}
{{- printf "%s-config" (include "aether.proxy.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.proxy.selectorLabels" -}}
app.kubernetes.io/name: aether-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: proxy
{{- end -}}
{{- define "aether.proxy.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.proxy.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* -------------------------------------------------------------- registrar */}}
{{- define "aether.registrar.fullname" -}}
{{- printf "%s-registrar" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.registrar.serviceAccountName" -}}{{ include "aether.registrar.fullname" . }}{{- end -}}
{{- define "aether.registrar.clusterScopedName" -}}
{{- printf "%s-%s" (include "aether.registrar.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.registrar.selectorLabels" -}}
app.kubernetes.io/name: aether-registrar
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: registrar
{{- end -}}
{{- define "aether.registrar.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.registrar.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* ------------------------------------------------------------------- edge */}}
{{- define "aether.edge.fullname" -}}
{{- printf "%s-edge" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- /* The edge is an ingress gateway, not a mesh workload — it runs in its own
       namespace (aether-ingress by default), isolated from the control plane. */ -}}
{{- define "aether.edge.namespace" -}}
{{- default "aether-ingress" .Values.edge.namespace -}}
{{- end -}}
{{- define "aether.edge.serviceAccountName" -}}{{ include "aether.edge.fullname" . }}{{- end -}}
{{- define "aether.edge.configMapName" -}}
{{- printf "%s-config" (include "aether.edge.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.edge.selectorLabels" -}}
app.kubernetes.io/name: aether-edge
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: edge
{{- end -}}
{{- define "aether.edge.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.edge.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* ------------------------------------------------------------- controller */}}
{{- define "aether.controller.fullname" -}}
{{- printf "%s-controller" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.controller.serviceAccountName" -}}{{ include "aether.controller.fullname" . }}{{- end -}}
{{- define "aether.controller.clusterScopedName" -}}
{{- printf "%s-%s" (include "aether.controller.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.controller.webhookServiceName" -}}
{{- printf "%s-webhook" (include "aether.controller.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.controller.selectorLabels" -}}
app.kubernetes.io/name: aether-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}
{{- define "aether.controller.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.controller.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* Name of the chart-shipped fleet-default EdgeConfig (proposal 029). */}}
{{- define "aether.edge.defaultConfigName" -}}
{{- default "aether-edge-defaults" .Values.edge.config.name -}}
{{- end -}}
