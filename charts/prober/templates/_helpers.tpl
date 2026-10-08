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
"true" when the live Namespace belongs to THIS Helm release (it carries the two
ownership annotations Helm stamps on what it creates or adopts). A copy of the
aether chart's aether.namespace.ownedByRelease (#1403/#1405; the charts are
packaged separately): a Namespace the release owns is in its stored manifest,
and Helm deletes an object that leaves the manifest, so an owned Namespace keeps
being rendered whatever namespace.create says. `lookup` returns nothing without
a cluster (`helm template`), so this is "" there.

Asked on an UPGRADE only, unlike the aether chart: a first install has no stored
manifest to protect, and this chart's default render is a DaemonSet and a
ServiceAccount, which an operator limited to one namespace may install; reading
a Namespace on install could refuse them for nothing.
*/}}
{{- define "prober.namespace.ownedByRelease" -}}
{{- if .Release.IsUpgrade -}}
{{- $annotations := dig "metadata" "annotations" (dict) (lookup "v1" "Namespace" "" (include "prober.namespace" .) | default (dict)) | default (dict) -}}
{{- if and (eq (get $annotations "meta.helm.sh/release-name" | toString) .Release.Name) (eq (get $annotations "meta.helm.sh/release-namespace" | toString) .Release.Namespace) -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
"true" or "false": does this render include the Namespace? Also stamped on the
ServiceAccount as aether.io/release-namespace-rendered for
prober.namespace.assertUpgradeKeeps to read on the next upgrade.
*/}}
{{- define "prober.namespace.rendered" -}}
{{- if or .Values.namespace.create (include "prober.namespace.ownedByRelease" .) -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/*
Fails an upgrade that could make Helm delete the namespace although this render
does not include it (a copy of aether.namespace.assertUpgradeKeeps, #1403/#1405;
the reasoning is spelled out there). The case: the release has no deployed
revision, and its failed revision (an older chart with namespace.create=true,
installed with --create-namespace) lists a Namespace it does not own. Helm
upgrades from that revision and deletes what left the manifest, unless the live
object says helm.sh/resource-policy: keep.
*/}}
{{- define "prober.namespace.assertUpgradeKeeps" -}}
{{- if .Release.IsUpgrade -}}
{{- $ns := include "prober.namespace" . -}}
{{- $live := lookup "v1" "Namespace" "" $ns | default (dict) -}}
{{- if and $live (ne (dig "metadata" "annotations" "helm.sh/resource-policy" "" $live | toString) "keep") -}}
{{- $revisions := 0 -}}
{{- $deployed := 0 -}}
{{- range (lookup "v1" "Secret" .Release.Namespace "" | default (dict)).items | default (list) -}}
{{- if and (eq (toString .type) "helm.sh/release.v1") (eq (dig "metadata" "labels" "name" "" . | toString) $.Release.Name) -}}
{{- $revisions = add1 $revisions -}}
{{- if eq (dig "metadata" "labels" "status" "" . | toString) "deployed" -}}
{{- $deployed = add1 $deployed -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $marker := dig "metadata" "annotations" "aether.io/release-namespace-rendered" "" (lookup "v1" "ServiceAccount" $ns (include "prober.serviceAccountName" .) | default (dict)) | toString -}}
{{- if and (gt $revisions 0) (eq $deployed 0) (ne $marker "false") -}}
{{- fail (printf "release %q has never been deployed successfully (none of its %d revisions is 'deployed'), and the failed first install of a prober chart older than 1.0.6 with namespace.create=true lists the namespace %q in its manifest: this render does not include that Namespace, so Helm could DELETE it, with every pod in it, on this upgrade. Protect the namespace, then run the same command again: kubectl annotate namespace %s helm.sh/resource-policy=keep" .Release.Name $revisions $ns $ns) -}}
{{- end -}}
{{- end -}}
{{- end -}}
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
Labels, in three sets (#1372; the same split as the aether chart's, #1363):

  prober.selectorLabels  name + instance + component. Immutable: they are the
                         DaemonSet's selector.
  prober.podLabels       the selector labels + part-of + managed-by. What the
                         POD TEMPLATE carries. Nothing here may change from one
                         release to the next: a pod template that changes rolls
                         every prober pod.
  prober.labels          the pod labels + helm.sh/chart + app.kubernetes.io/
                         version. What an OBJECT carries on its own metadata
                         (the DaemonSet, the ServiceAccount, the Namespace).

Until chart 1.0.5 the pod template used `labels`, and this chart's version
carries the commit, so every release rolled every prober pod.
//charts/prober:prober_pod_template_version_test renders the chart at two
versions and fails if a pod template differs.
*/}}
{{- define "prober.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prober.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: prober
{{- end -}}
{{- define "prober.podLabels" -}}
{{ include "prober.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "prober.labels" -}}
helm.sh/chart: {{ include "prober.chart" . }}
{{ include "prober.podLabels" . }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Labels of the authz-canary OBJECTS (its two ServiceAccounts, two Deployments
and the HTTPFilter), on their own metadata (#1373). The same as prober.labels
except for the component: with `component: prober` the canary objects matched
the prober DaemonSet's selector labels, so `-l app.kubernetes.io/component=prober`
counted them as the prober.

Object metadata only. The canary Deployments select on `app: authz-echo` /
`app: authz-canary` and their pod templates carry that label and nothing from
here: a Deployment's selector is immutable, and nothing here may reach a pod.
*/}}
{{- define "prober.authzCanary.labels" -}}
helm.sh/chart: {{ include "prober.chart" . }}
app.kubernetes.io/name: {{ include "prober.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: authz-canary
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
