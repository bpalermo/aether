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
"true" when the live Namespace `name` belongs to THIS Helm release, i.e. carries
the two ownership annotations Helm stamps on every object it creates or adopts.
That is the case for a release installed while namespace.create still defaulted
to true (chart 2.4.x and older).

It is what makes the namespace templates upgrade-safe (#1403): a Namespace the
release owns is in the release's stored manifest, and Helm DELETES an object
that was in the previous manifest and is not in the new one — here, the
namespace with every aether pod in it. So an owned Namespace keeps being
rendered whatever the values say.

Annotations can be lost (an object replaced from a file, a tool that prunes
them). A Namespace an older chart rendered would then look unowned on the very
upgrade that drops it from the manifest, before `keep` was ever stamped. So on
an upgrade a second sign counts too: the labels every version of this chart has
put on the Namespace it renders (app.kubernetes.io/managed-by: Helm and
app.kubernetes.io/instance: <this release>), on a namespace that does not carry
helm.sh/resource-policy: keep yet. Helm patches a Namespace that is in the
previous manifest without asking who owns it, and stamps ownership and `keep`
back. (With `keep` already there the namespace is safe either way, and one that
has left the manifest must not be rendered again: Helm would refuse to adopt
it.) A namespace that lost the annotations AND those labels cannot be told from
one Helm or the operator created; docs/runbook.md, "Chart 2.4.21", says what to
check before upgrading.

`lookup` returns nothing without a cluster (`helm template`, a client-side
`--dry-run`), so this is "" there: such a render shows what a first install
would get.
Usage: include "aether.namespace.ownedByRelease" (dict "ctx" . "name" $ns)
*/}}
{{- define "aether.namespace.ownedByRelease" -}}
{{- $live := lookup "v1" "Namespace" "" .name | default (dict) -}}
{{- $annotations := dig "metadata" "annotations" (dict) $live | default (dict) -}}
{{- $labels := dig "metadata" "labels" (dict) $live | default (dict) -}}
{{- if and (eq (get $annotations "meta.helm.sh/release-name" | toString) .ctx.Release.Name) (eq (get $annotations "meta.helm.sh/release-namespace" | toString) .ctx.Release.Namespace) -}}
true
{{- else if and .ctx.Release.IsUpgrade (eq (get $labels "app.kubernetes.io/managed-by" | toString) "Helm") (eq (get $labels "app.kubernetes.io/instance" | toString) .ctx.Release.Name) (ne (get $annotations "helm.sh/resource-policy" | toString) "keep") -}}
true
{{- end -}}
{{- end -}}

{{/*
"true" or "false": does this render include the release's Namespace? True when
namespace.create asks for it, or when the live one already belongs to the
release (see aether.namespace.ownedByRelease). Also stamped on the agent
ServiceAccount as aether.io/release-namespace-rendered, which
aether.namespace.assertUpgradeKeeps reads on the next upgrade.
*/}}
{{- define "aether.namespace.rendered" -}}
{{- if or .Values.namespace.create (include "aether.namespace.ownedByRelease" (dict "ctx" . "name" (include "aether.namespace" .))) -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/*
Fails an upgrade that could make Helm delete the namespace although this render
does not include it (#1403).

Helm deletes an object that was in the previous revision's manifest and is not
in the new one; it consults nothing but helm.sh/resource-policy on the live
object. A render cannot read the previous manifest, so the Namespace is only
left out of an UPGRADE when the chart can see that this is safe:
  - the live namespace already carries helm.sh/resource-policy: keep; or
  - the agent ServiceAccount says the previous revision was rendered by a chart
    that itself left the Namespace out (aether.io/release-namespace-rendered:
    "false", stamped since 2.4.21), so the stored manifest holds none.
Otherwise it stops, with the one command that makes the upgrade safe. That
covers, without telling them apart:
  - a first install of a chart older than 2.4.21 that failed with
    `--create-namespace` ("already exists": its manifest lists a Namespace the
    release never owned, and Helm upgrades from that failed revision);
  - a release of an older chart whose Namespace lost both Helm's ownership
    annotations and the chart's labels (aether.namespace.ownedByRelease no
    longer recognises it);
  - a release of an older chart installed with namespace.create=false, whose
    manifest never held the Namespace. This one is safe, but looks the same
    from here, so it is asked for the same command ONCE: the first upgrade to
    this chart stamps the marker, and later upgrades pass.

It is also asked BEFORE aether.namespace.assertCreatable when namespace.create
is true: the failed installs it is about were made with namespace.create=true,
and a retry of that same command must get this message, with `keep` in it, and
not first a "cannot create" one that sends the operator to namespace.create=false
and into a second failure (#1405).
Usage: include "aether.namespace.assertUpgradeKeeps" .
*/}}
{{- define "aether.namespace.assertUpgradeKeeps" -}}
{{- if .Release.IsUpgrade -}}
{{- $ns := include "aether.namespace" . -}}
{{- $live := lookup "v1" "Namespace" "" $ns | default (dict) -}}
{{- if and $live (ne (dig "metadata" "annotations" "helm.sh/resource-policy" "" $live | toString) "keep") -}}
{{- $marker := dig "metadata" "annotations" "aether.io/release-namespace-rendered" "" (lookup "v1" "ServiceAccount" $ns (include "aether.agent.serviceAccountName" .) | default (dict)) | toString -}}
{{- if ne $marker "false" -}}
{{- fail (printf "this upgrade of release %q does not render the namespace %q, and the chart cannot see that the previous revision did not either (it was written by an aether chart older than 2.4.21, or it rendered the Namespace). If the previous manifest lists the Namespace, Helm could DELETE it, with every pod in it. Protect the namespace, then run the same command again (without namespace.create=true, if you pass it: a chart cannot create the namespace its own release is stored in): kubectl annotate namespace %s helm.sh/resource-policy=keep (harmless if the release never rendered the namespace, and needed once only; if the namespace is not labelled for Pod Security admission yet: kubectl label namespace %s --overwrite %s). See docs/runbook.md, \"Chart 2.4.21\"." .Release.Name $ns $ns $ns "pod-security.kubernetes.io/enforce=privileged pod-security.kubernetes.io/audit=privileged pod-security.kubernetes.io/warn=privileged") -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Fails the render when the chart is asked to create the namespace its own release
is stored in (#1403). Helm writes the release record into the release namespace
before it creates anything the chart renders, so that combination cannot be
installed by any helm command: with --create-namespace the chart's Namespace
collides with the one Helm just made ("already exists"), without it there is
nowhere to put the release ("not found"), and a namespace made with kubectl is
refused ("invalid ownership metadata"). Failing here says so, with the ways out,
instead of leaving a failed release behind.

A Namespace the release already owns is the exception (an install from before
the default changed, or a namespace pre-created with Helm's ownership metadata):
that one is adopted or simply kept.
Usage: include "aether.namespace.assertCreatable" (dict "ctx" . "name" $ns "value" "namespace.create" "waysOut" "...")
*/}}
{{- define "aether.namespace.assertCreatable" -}}
{{- if and (eq .name .ctx.Release.Namespace) (not (include "aether.namespace.ownedByRelease" .)) -}}
{{- fail (printf "%s=true cannot create %q: it is the namespace this release is stored in, and Helm writes the release record there before it creates anything the chart renders, so no helm command can install this. Either %s. See docs/getting-started.md, \"Install\"." .value .name .waysOut) -}}
{{- end -}}
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
Every container in this chart renders its resources through this helper
(#1355): a plain `toYaml` on a resources value brings `cpu: ""` back.
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

{{/*
The `topologySpreadConstraints:` key of a multi-replica Deployment's pod spec
(the registrar, the controller, the edge; #1434), or nothing.

  <component>.topologySpreadConstraints   non-empty: rendered as given. It
                                          REPLACES the chart's constraint, it is
                                          not merged with it, and nodeSpread is
                                          then not consulted beyond validation.
  <component>.nodeSpread                  soft      one constraint over
                                                    kubernetes.io/hostname,
                                                    maxSkew 1, ScheduleAnyway
                                          required  the same, DoNotSchedule
                                          none      no constraint

What the default is built from was measured, not reasoned (kind, Kubernetes
1.35, one tainted control plane plus two or three workers, the three
Deployments rolled together twenty times as a chart upgrade rolls them;
rollouts that ended with both replicas of a Deployment on one node, as
controller / edge / registrar):

                                               2 workers      3 workers
  chart 2.4.18                                 8 / 5 / 10     1 / 0 / 5
  the same without the controller's
    "off the registrars' node" term            0 / 6 / 2      0 / 0 / 1
  this constraint, that term kept              9 / 0 / 11     1 / 0 / 3
  this constraint, that term gone (2.4.20)     0 / 0 / 0      0 / 0 / 0

Two causes, then. The larger one was the controller's default preferred
anti-affinity against the registrars' node (controller-deployment.yaml says
why it cancelled the spread). The smaller one is what
`matchLabelKeys: [pod-template-hash]` removes: without it the constraint
counts the old ReplicaSet's pods beside the new ones during a rollout. The
field is on by default for spread constraints from Kubernetes 1.27 (for pod
anti-affinity only from 1.31, one reason this is a spread constraint).

The other reason: `required` cannot wedge a small cluster. DoNotSchedule with
maxSkew 1 refuses a node only while another counted node holds fewer
replicas, so two replicas on a single node, or three on one, still schedule
(run on kind). Required pod anti-affinity leaves every replica beyond the node
count Pending, and a surging rollout with it.

`required` adds `nodeTaintsPolicy: Honor`: a tainted node the pod does not
tolerate (a control plane) is left out of the count. Counted, it is an empty
node nothing can be put on, and DoNotSchedule then refuses every node that
already holds one replica: on kind, the third of three replicas stayed
Pending on two workers without it. The price: a node that is NotReady or
cordoned is tainted too (node.kubernetes.io/not-ready; a cordon gets
node.kubernetes.io/unschedulable from the node controller), so while it is,
replicas may share another node. Run on kind with one of two workers
cordoned: with Honor both replicas ran on the other worker; without it the
second stayed Pending.

Below Kubernetes 1.27 the apiserver drops matchLabelKeys. `soft` renders
without it there (a preference either way). `required` fails the render
instead: what would be left is a DoNotSchedule rule that does not keep a
rollout's new replicas apart.

Usage (the selector labels are the component's own; .Release is not reachable
from the values dict):
  {{- with include "aether.nodeSpread" (dict "ctx" . "name" "registrar" "values" .Values.registrar "selectorLabels" (include "aether.registrar.selectorLabels" .)) }}
  {{- . | nindent 6 }}
  {{- end }}
*/}}
{{- define "aether.nodeSpread" -}}
{{- $mode := .values.nodeSpread -}}
{{- if not (has $mode (list "soft" "required" "none")) -}}
{{- fail (printf "%s.nodeSpread must be one of soft, required, none; got %v (an unquoted `off` or `no` in a values file is the YAML boolean false: write none)" .name $mode) -}}
{{- end -}}
{{- $kube := .ctx.Capabilities.KubeVersion.Version -}}
{{- $perReplicaSet := semverCompare ">=1.27.0-0" $kube -}}
{{- if .values.topologySpreadConstraints -}}
topologySpreadConstraints:
  {{- toYaml .values.topologySpreadConstraints | nindent 2 }}
{{- else if ne $mode "none" -}}
{{- if and (eq $mode "required") (not $perReplicaSet) -}}
{{- fail (printf "%s.nodeSpread=required requires Kubernetes >= 1.27 (matchLabelKeys on a topology spread constraint; without it a rollout's old replicas are counted with the new ones), got %s; with `helm template` pass --kube-version, or write the constraint yourself in %s.topologySpreadConstraints" .name $kube .name) -}}
{{- end -}}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: {{ ternary "DoNotSchedule" "ScheduleAnyway" (eq $mode "required") }}
    labelSelector:
      matchLabels:
        {{- .selectorLabels | nindent 8 }}
    {{- if eq $mode "required" }}
    nodeTaintsPolicy: Honor
    {{- end }}
    {{- if $perReplicaSet }}
    matchLabelKeys:
      - pod-template-hash
    {{- end }}
{{- end -}}
{{- end -}}

{{/*
Labels, three sets per component (#1363):

  aether.<c>.selectorLabels  name + instance + component. Immutable: they are
                             the workload's selector.
  aether.<c>.podLabels       the selector labels + part-of + managed-by. What a
                             POD TEMPLATE carries. Nothing here may change from
                             one release to the next: a pod template that
                             changes rolls every pod of the workload, and for
                             the proxy that is a hot restart on every node.
  aether.<c>.labels          the pod labels + helm.sh/chart + app.kubernetes.io/
                             version. What an OBJECT carries on its own
                             metadata (the DaemonSet, the Service, the RBAC).

Until chart 2.4.15 the pod templates used `labels`, so `helm.sh/chart:
aether-<version>` (and, on a stamped build, the `git describe` appVersion) put
the release into every pod template and every chart release rolled every pod.
//charts/aether:aether_pod_template_version_test renders the chart at two
versions and fails if a pod template differs.
*/}}

{{/* Chart label value (name-version). Never on a pod template: see above. */}}
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
{{- define "aether.agent.podLabels" -}}
{{ include "aether.agent.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.agent.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.agent.podLabels" . }}
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
{{- define "aether.meshDns.podLabels" -}}
{{ include "aether.meshDns.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.meshDns.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.meshDns.podLabels" . }}
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
{{- define "aether.udsCsi.podLabels" -}}
{{ include "aether.udsCsi.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.udsCsi.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.udsCsi.podLabels" . }}
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

{{- define "aether.proxy.podLabels" -}}
{{ include "aether.proxy.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.proxy.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.proxy.podLabels" . }}
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
{{- define "aether.registrar.podLabels" -}}
{{ include "aether.registrar.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.registrar.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.registrar.podLabels" . }}
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
{{- define "aether.edge.podLabels" -}}
{{ include "aether.edge.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.edge.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.edge.podLabels" . }}
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
{{/*
"The pod-mutating webhook exists": non-empty ("true") when the chart renders the
MutatingWebhookConfiguration, empty when it does not. It has two entries, one
per switch, so it renders when EITHER is on.

Everything that depends on that object existing asks this helper and nothing
else (#1411: the RBAC rule used to ask injectPodNdots alone, so namespace
injection without ndots told the controller to patch an object it had no
permission to read):

  controller-webhook.yaml     renders the MutatingWebhookConfiguration
  controller-deployment.yaml  passes --mutating-webhook-config-name (SPIRE-served
                              webhook only: its caBundle starts empty and the
                              controller fills it)
  controller-rbac.yaml        grants mutatingwebhookconfigurations (same)

//charts/aether:aether_mutating_webhook_*_test render every combination.

Use it as `{{ if include "aether.controller.mutatingWebhook" . }}`.
*/}}
{{- define "aether.controller.mutatingWebhook" -}}
{{- if or .Values.controller.namespaceInjection .Values.controller.injectPodNdots -}}true{{- end -}}
{{- end -}}
{{/*
Its name, for the object and for the flag that tells the controller which one to
patch. "-pod-ndots" is historical (it also carries namespace injection);
renaming it would delete and recreate the object on upgrade.
*/}}
{{- define "aether.controller.mutatingWebhookName" -}}
{{- include "aether.controller.clusterScopedName" . }}-pod-ndots
{{- end -}}
{{- define "aether.controller.selectorLabels" -}}
app.kubernetes.io/name: aether-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}
{{- define "aether.controller.podLabels" -}}
{{ include "aether.controller.selectorLabels" . }}
app.kubernetes.io/part-of: aether
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "aether.controller.labels" -}}
helm.sh/chart: {{ include "aether.chart" . }}
{{ include "aether.controller.podLabels" . }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* Name of the chart-shipped fleet-default EdgeConfig (proposal 029). */}}
{{- define "aether.edge.defaultConfigName" -}}
{{- default "aether-edge-defaults" .Values.edge.config.name -}}
{{- end -}}
