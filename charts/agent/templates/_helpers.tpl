{{/*
Chart name, optionally overridden via nameOverride.
*/}}
{{- define "aether-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified, release-prefixed name. Used for namespaced resource names so
multiple releases can coexist. Honours fullnameOverride / nameOverride.
*/}}
{{- define "aether-agent.fullname" -}}
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
Proxy fullname.
*/}}
{{- define "aether-agent.proxy.fullname" -}}
{{- printf "%s-proxy" (include "aether-agent.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Namespace the chart deploys into. Defaults to the release namespace.
*/}}
{{- define "aether-agent.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
Name for cluster-scoped resources (ClusterRole/ClusterRoleBinding). Includes the
namespace so two releases of the same name in different namespaces don't collide.
*/}}
{{- define "aether-agent.clusterScopedName" -}}
{{- printf "%s-%s" (include "aether-agent.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
ServiceAccount names.
*/}}
{{- define "aether-agent.serviceAccountName" -}}
{{- include "aether-agent.fullname" . -}}
{{- end -}}

{{- define "aether-agent.proxy.serviceAccountName" -}}
{{- include "aether-agent.proxy.fullname" . -}}
{{- end -}}

{{/*
Proxy config ConfigMap name.
*/}}
{{- define "aether-agent.proxy.configMapName" -}}
{{- printf "%s-config" (include "aether-agent.proxy.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
AWS credentials Secret name. Defaults to a release-derived name.
*/}}
{{- define "aether-agent.awsSecretName" -}}
{{- default (printf "%s-aws-credentials" (include "aether-agent.fullname" .)) .Values.awsCredentials.secretName -}}
{{- end -}}

{{/*
Chart label value (name-version).
*/}}
{{- define "aether-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Agent selector labels (immutable subset).
*/}}
{{- define "aether-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aether-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end -}}

{{/*
Agent common labels.
*/}}
{{- define "aether-agent.labels" -}}
helm.sh/chart: {{ include "aether-agent.chart" . }}
{{ include "aether-agent.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Proxy selector labels (immutable subset).
*/}}
{{- define "aether-agent.proxy.selectorLabels" -}}
app.kubernetes.io/name: aether-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: proxy
{{- end -}}

{{/*
Proxy common labels.
*/}}
{{- define "aether-agent.proxy.labels" -}}
helm.sh/chart: {{ include "aether-agent.chart" . }}
{{ include "aether-agent.proxy.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
