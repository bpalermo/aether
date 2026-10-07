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

{{/*
GOMEMLIMIT for a Go container, as an integer byte count: 90% of the container's
memory limit. Takes the container's `resources` dict; renders "" (the caller
omits the env var) when no memory limit is set.

Never the limit itself. `resourceFieldRef: limits.memory` (divisor 1) handed the
Go runtime the WHOLE cgroup, so the heap could grow to the limit before the GC
tightened, leaving nothing for everything else charged to the same cgroup: exec
probe processes (proposal 041), runc's exec helper, kernel socket buffers, page
cache. The 2026-10-04 agent OOMKills were exactly that.

Accepts plain or decimal numbers (`128Mi`, `1.5Gi`, `134217728`) with the
Ki/Mi/Gi/Ti and k/M/G/T suffixes; anything else fails the render rather than
silently dropping the limit.
Usage:
  {{- with include "aether.goMemLimit" .Values.agent.resources }}
  - name: GOMEMLIMIT
    value: {{ . | quote }}
  {{- end }}
*/}}
{{- define "aether.goMemLimit" -}}
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
{{- /* x9 first, then /10: exact in float64 for any realistic limit, so an
       exact 90% is never floored one byte low. */}}
{{- divf (mulf $n (get $units $unit) 9) 10 | floor | int64 -}}
{{- end -}}
{{- end -}}

{{/*
A container's `resources` block with empty quantities dropped (#1253, #1321).
Takes the container's `resources` dict and renders it as YAML.

An empty or null quantity is omitted rather than rendered as `cpu: ""`, which
the apiserver rejects as a quantity. So `--set <x>.resources.limits.cpu=` (or
`limits: {cpu: null}` in a values file) means "no CPU limit", exactly like
leaving the key out, and a `limits:`/`requests:` map left with no entries is
dropped too. Non-map keys (e.g. `claims`) pass through untouched.
Usage:
  resources:
    {{- include "aether.resources" .Values.udsCsi.resources | nindent 12 }}
*/}}
{{- define "aether.resources" -}}
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

{{/* --------------------------------------------------------------- uds-csi */}}
{{- define "aether.udsCsi.fullname" -}}
{{- printf "%s-uds-csi" (include "aether.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "aether.udsCsi.selectorLabels" -}}
app.kubernetes.io/name: aether-uds-csi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: uds-csi
{{- end -}}
{{- define "aether.udsCsi.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.udsCsi.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}
{{/*
udsCsi.kubeletRoot, validated and without a trailing slash. It is the ONLY
place the kubelet root appears on the CSI path (proposal 039, R3): the
DaemonSet's hostPaths, its mountPaths (identical, because the kubelet hands the
plugin HOST paths) and the plugin's --kubelet-root all derive from it, so they
cannot disagree.
*/}}
{{- define "aether.udsCsi.kubeletRoot" -}}
{{- $r := .Values.udsCsi.kubeletRoot | toString | trimSuffix "/" -}}
{{- if not (hasPrefix "/" $r) -}}
{{- fail (printf "udsCsi.kubeletRoot must be an absolute path, got %q" .Values.udsCsi.kubeletRoot) -}}
{{- end -}}
{{- $r -}}
{{- end -}}
{{/*
udsCsi.inodes as a plain integer (a YAML number is a float64 to the template
engine, and 1e+06 is not a flag value), refused below the plugin's minimum of 8
so a bad value fails the install rather than crash-looping the DaemonSet.
*/}}
{{- define "aether.udsCsi.inodes" -}}
{{- $n := .Values.udsCsi.inodes | int64 -}}
{{- if lt $n 8 -}}
{{- fail (printf "udsCsi.inodes must be at least 8, got %v" .Values.udsCsi.inodes) -}}
{{- end -}}
{{- $n -}}
{{- end -}}

{{- define "aether.udsCsi.root" -}}
{{- $r := .Values.udsCsi.root | toString | trimSuffix "/" -}}
{{- if not (hasPrefix "/" $r) -}}
{{- fail (printf "udsCsi.root must be an absolute path, got %q" .Values.udsCsi.root) -}}
{{- end -}}
{{- $r -}}
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
{{/*
proxy.authzSidecar.failureMode, normalised and validated (#770).

This is an authorization control: DENY fails closed (the proxy answers 403 when
the sidecar is unreachable), ALLOW fails open. The consumer used to compare the
raw value with `eq ... "ALLOW"`, so `allow`, `Allow` or a stray trailing space
selected fail-CLOSED silently — no error, no warning, nothing in `helm lint`.
Normalise first, then reject anything that is neither spelling rather than
guessing a failure mode on the operator's behalf.
*/}}
{{- define "aether.proxy.authzFailureMode" -}}
{{- $mode := upper (trim (toString .Values.proxy.authzSidecar.failureMode)) -}}
{{- if not (has $mode (list "ALLOW" "DENY")) -}}
{{- fail (printf "proxy.authzSidecar.failureMode must be ALLOW or DENY, got %q" (toString .Values.proxy.authzSidecar.failureMode)) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{/*
aether.durationMillis: a Go duration string made of whole-number ms/s/m/h
components ("15s", "1m30s", "500ms") in milliseconds. Fails the render on
anything else, naming the value, rather than guessing. Takes
(dict "name" <values path> "value" <string>).
*/}}
{{- define "aether.durationMillis" -}}
{{- $s := trim (toString .value) -}}
{{- $parts := regexFindAll "[0-9]+(ms|s|m|h)" $s -1 -}}
{{- if or (not $parts) (ne (join "" $parts) $s) -}}
{{- fail (printf "%s must be a duration of whole ms/s/m/h components like 15s, 1m30s or 500ms, got %q" .name $s) -}}
{{- end -}}
{{- $unit := dict "ms" 1 "s" 1000 "m" 60000 "h" 3600000 -}}
{{- $total := 0 -}}
{{- range $parts -}}
{{- $total = add $total (mul (atoi (regexFind "^[0-9]+" .)) (get $unit (regexReplaceAll "^[0-9]+" . ""))) -}}
{{- end -}}
{{- $total -}}
{{- end -}}

{{/*
proxy.hotRestart.drainStrategy, validated (#1054): Envoy's --drain-strategy
accepts immediate or gradual and aborts on anything else, which would
crash-loop every proxy pod at rollout time instead of failing here.
*/}}
{{- define "aether.proxy.drainStrategy" -}}
{{- $s := trim (toString .Values.proxy.hotRestart.drainStrategy) -}}
{{- if not (has $s (list "immediate" "gradual")) -}}
{{- fail (printf "proxy.hotRestart.drainStrategy must be immediate or gradual, got %q" $s) -}}
{{- end -}}
{{- $s -}}
{{- end -}}

{{/*
proxy.concurrency, validated (#1093): Envoy's --concurrency takes a positive
integer. 0 renders nothing (Envoy's one-worker-per-core default); a negative or
non-integer value fails the render instead of crash-looping every proxy pod.
Renders the integer when > 0, else the empty string.
*/}}
{{- define "aether.proxy.concurrency" -}}
{{- include "aether.envoyConcurrency" (dict "key" "proxy.concurrency" "value" .Values.proxy.concurrency) -}}
{{- end -}}

{{/*
edge.concurrency, validated (#1344): the same contract as proxy.concurrency,
for the edge Deployment's `envoy` container. 0 renders nothing (one worker per
core); a negative or non-integer value fails the render.
*/}}
{{- define "aether.edge.concurrency" -}}
{{- include "aether.envoyConcurrency" (dict "key" "edge.concurrency" "value" .Values.edge.concurrency) -}}
{{- end -}}

{{/*
The one validator behind both: takes (dict "key" <values path, for the error>
"value" <the value>) and renders the integer when > 0, else the empty string.
Only an unset (null) value reads as 0. `default 0` would also swallow `false`
and the empty string, turning a mistyped value into "one worker per core"
instead of an error.
*/}}
{{- define "aether.envoyConcurrency" -}}
{{- $s := ternary "0" (toString .value) (kindIs "invalid" .value) -}}
{{- if not (regexMatch "^[0-9]+$" $s) -}}
{{- fail (printf "%s must be a non-negative integer, got %q" .key $s) -}}
{{- end -}}
{{- if gt (atoi $s) 0 -}}
{{- atoi $s -}}
{{- end -}}
{{- end -}}

{{/*
agent.eastWestQuicIdleTimeout, validated against the proxy's parent-shutdown
time (#1054). A source h3 connection that is idle when a destination proxy's
hot-restart drain starts gets no GOAWAY; it must be closed by this idle timeout
before the parent exits (parentShutdownTime after the fork), or its next packet
reaches the child and draws a stateless reset. The 5s margin covers the
child's startup before the drain begins and a request that re-arms the timer
just after it. Checked here because the agent never sees the proxy
DaemonSet's value.
*/}}
{{- define "aether.agent.eastWestQuicIdleTimeout" -}}
{{- $idle := include "aether.durationMillis" (dict "name" "agent.eastWestQuicIdleTimeout" "value" .Values.agent.eastWestQuicIdleTimeout) | atoi -}}
{{- $pst := include "aether.durationMillis" (dict "name" "proxy.hotRestart.parentShutdownTime" "value" .Values.proxy.hotRestart.parentShutdownTime) | atoi -}}
{{- if le $idle 0 -}}
{{- fail (printf "agent.eastWestQuicIdleTimeout must be > 0, got %q" (toString .Values.agent.eastWestQuicIdleTimeout)) -}}
{{- end -}}
{{- if ge (add $idle 5000) $pst -}}
{{- fail (printf "agent.eastWestQuicIdleTimeout (%s) + 5s must be below proxy.hotRestart.parentShutdownTime (%s): an idle h3 connection must close before a destination's hot-restart parent exits (#1054)" (toString .Values.agent.eastWestQuicIdleTimeout) (toString .Values.proxy.hotRestart.parentShutdownTime)) -}}
{{- end -}}
{{- trim (toString .Values.agent.eastWestQuicIdleTimeout) -}}
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
{{/*
The registrar's in-cluster gRPC address, built from THIS release's registrar
Service (name, namespace, port). The agent and edge binaries default to
aether-registrar.aether-system.svc:443, which is only right for a release named
"aether" in aether-system with the default port, so both are always handed it.
*/}}
{{- define "aether.registrar.address" -}}
{{- printf "%s.%s.svc:%v" (include "aether.registrar.fullname" .) (include "aether.namespace" .) .Values.registrar.service.port -}}
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
