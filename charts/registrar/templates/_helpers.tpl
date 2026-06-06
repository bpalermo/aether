{{/*
Chart name, optionally overridden via nameOverride.
*/}}
{{- define "aether-registrar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified, release-prefixed name. Used for namespaced resource names so
multiple releases can coexist. Honours fullnameOverride / nameOverride.
*/}}
{{- define "aether-registrar.fullname" -}}
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
{{- define "aether-registrar.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
Name for cluster-scoped resources (ClusterRole/ClusterRoleBinding). Includes the
namespace so two releases of the same name in different namespaces don't collide.
*/}}
{{- define "aether-registrar.clusterScopedName" -}}
{{- printf "%s-%s" (include "aether-registrar.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "aether-registrar.serviceAccountName" -}}
{{- include "aether-registrar.fullname" . -}}
{{- end -}}

{{/*
Chart label value (name-version).
*/}}
{{- define "aether-registrar.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels (immutable subset).
*/}}
{{- define "aether-registrar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aether-registrar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: registrar
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "aether-registrar.labels" -}}
helm.sh/chart: {{ include "aether-registrar.chart" . }}
{{ include "aether-registrar.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
