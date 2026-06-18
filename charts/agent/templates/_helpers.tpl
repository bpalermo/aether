{{/*
Chart name, optionally overridden via nameOverride.
*/}}
{{- define "agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified, release-prefixed name. Used for namespaced resource names so
multiple releases can coexist. Honours fullnameOverride / nameOverride.
*/}}
{{- define "agent.fullname" -}}
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
Proxy fullname. The Envoy proxy is a distinct "aether-proxy" component — its
selector labels and Envoy --service-cluster already use that name — so its
DaemonSet, ServiceAccount and ConfigMap are named aether-proxy rather than
"<agent-fullname>-proxy".
*/}}
{{- define "agent.proxy.fullname" -}}
{{- "aether-proxy" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Namespace the chart deploys into. Defaults to the release namespace.
*/}}
{{- define "agent.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
Name for cluster-scoped resources (ClusterRole/ClusterRoleBinding). Includes the
namespace so two releases of the same name in different namespaces don't collide.
*/}}
{{- define "agent.clusterScopedName" -}}
{{- printf "%s-%s" (include "agent.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
ServiceAccount names.
*/}}
{{- define "agent.serviceAccountName" -}}
{{- include "agent.fullname" . -}}
{{- end -}}

{{- define "agent.proxy.serviceAccountName" -}}
{{- include "agent.proxy.fullname" . -}}
{{- end -}}

{{/*
Proxy config ConfigMap name.
*/}}
{{- define "agent.proxy.configMapName" -}}
{{- printf "%s-config" (include "agent.proxy.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
AWS credentials Secret name. Defaults to a release-derived name.
*/}}
{{- define "agent.awsSecretName" -}}
{{- default (printf "%s-aws-credentials" (include "agent.fullname" .)) .Values.awsCredentials.secretName -}}
{{- end -}}

{{/*
Chart label value (name-version).
*/}}
{{- define "agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Agent selector labels (immutable subset).
*/}}
{{- define "agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end -}}

{{/*
Agent common labels.
*/}}
{{- define "agent.labels" -}}
helm.sh/chart: {{ include "agent.chart" . }}
{{ include "agent.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Proxy selector labels (immutable subset).
*/}}
{{- define "agent.proxy.selectorLabels" -}}
app.kubernetes.io/name: aether-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: proxy
{{- end -}}

{{/*
Proxy common labels.
*/}}
{{- define "agent.proxy.labels" -}}
helm.sh/chart: {{ include "agent.chart" . }}
{{ include "agent.proxy.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Name of the MeshConfig ConfigMap, projected by the aether-controller and mounted
by the agent. Derived from the release name (a convention shared with the
controller and registrar charts), so the agent carries no MeshConfig values.
*/}}
{{- define "agent.meshConfigMapName" -}}
{{- printf "%s-mesh-config" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
