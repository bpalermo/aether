{{/*
Chart name, optionally overridden via nameOverride.
*/}}
{{- define "registrar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified, release-prefixed name. Used for namespaced resource names so
multiple releases can coexist. Honours fullnameOverride / nameOverride.
*/}}
{{- define "registrar.fullname" -}}
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
{{- define "registrar.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
Name for cluster-scoped resources (ClusterRole/ClusterRoleBinding). Includes the
namespace so two releases of the same name in different namespaces don't collide.
*/}}
{{- define "registrar.clusterScopedName" -}}
{{- printf "%s-%s" (include "registrar.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "registrar.serviceAccountName" -}}
{{- include "registrar.fullname" . -}}
{{- end -}}

{{/*
Chart label value (name-version).
*/}}
{{- define "registrar.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels (immutable subset).
*/}}
{{- define "registrar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "registrar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: registrar
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "registrar.labels" -}}
helm.sh/chart: {{ include "registrar.chart" . }}
{{ include "registrar.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
