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
GOMEMLIMIT as an integer byte count: 90% of the container's memory limit (a
copy of the aether chart's aether.goMemLimit; the charts are packaged
separately). Takes the container's `resources` dict; renders "" (the caller
omits the env var) when no memory limit is set. Never the limit itself: a
`resourceFieldRef: limits.memory` hands the Go heap the whole cgroup and leaves
nothing for the rest of it (the 2026-10-04 agent OOMKills).
*/}}
{{- define "prober.goMemLimit" -}}
{{- $q := dig "limits" "memory" "" (default (dict) .) -}}
{{- if $q -}}
{{- $s := "" -}}
{{- if or (kindIs "float64" $q) (kindIs "int" $q) (kindIs "int64" $q) -}}
{{- $s = int64 $q | toString -}}
{{- else -}}
{{- $s = toString $q | trim -}}
{{- end -}}
{{- if not (regexMatch "^[0-9]+(\\.[0-9]+)?(Ki|Mi|Gi|Ti|k|M|G|T)?$" $s) -}}
{{- fail (printf "cannot derive GOMEMLIMIT from resources.limits.memory %q: use a number with an optional Ki/Mi/Gi/Ti or k/M/G/T suffix" $s) -}}
{{- end -}}
{{- $units := dict "" 1 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 -}}
{{- $n := regexReplaceAll "^([0-9.]+).*$" $s "${1}" | float64 -}}
{{- $unit := regexReplaceAll "^[0-9.]+" $s "" -}}
{{- divf (mulf $n (get $units $unit) 9) 10 | floor | int64 -}}
{{- end -}}
{{- end -}}

{{/*
A container's `resources` block with empty quantities dropped (#1361; a copy of
the aether chart's aether.resources, the charts are packaged separately).
Takes the container's `resources` dict and renders it as YAML.

An empty or null quantity is omitted rather than rendered as `cpu: ""`, which
the apiserver rejects as a quantity. So `--set resources.limits.cpu=` (or
`limits: {cpu: null}` in a values file) means "no CPU limit", exactly like
leaving the key out, and a `limits:`/`requests:` map left with no entries is
dropped too. Non-map keys (e.g. `claims`) pass through untouched.
Every container in this chart renders its resources through this helper: a
plain `toYaml` on a resources value brings `cpu: ""` back.
Usage:
  resources:
    {{- include "prober.resources" .Values.resources | nindent 12 }}
*/}}
{{- define "prober.resources" -}}
{{- $res := dict -}}
{{- range $kind, $list := (. | default dict) -}}
{{- if kindIs "map" $list -}}
{{- $kept := dict -}}
{{- range $name, $q := $list -}}
{{- if not (or (kindIs "invalid" $q) (eq (toString $q) "")) -}}
{{- $_ := set $kept $name $q -}}
{{- end -}}
{{- end -}}
{{- if $kept -}}
{{- $_ := set $res $kind $kept -}}
{{- end -}}
{{- else if $list -}}
{{- $_ := set $res $kind $list -}}
{{- end -}}
{{- end -}}
{{- toYaml $res -}}
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
