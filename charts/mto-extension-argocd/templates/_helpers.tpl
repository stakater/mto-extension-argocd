{{- define "mto-extension-argocd.childTemplateName" -}}
mto-argocd-child
{{- end -}}

{{- define "mto-extension-argocd.parentTemplateName" -}}
mto-argocd-parent
{{- end -}}

{{- /*
The RBAC must OUTLIVE the TemplateInstances on teardown: tov2's finalizers need
this chart's grants to release/delete the AppProjects and reap the children, so
if the RBAC goes first everything deadlocks in Terminating. Retained
under both tear-down tools, which honour different annotations: resource-policy
for `helm uninstall`, sync-options (Prune=false,Delete=false) for an ArgoCD app
prune/delete. Harmless once the projects drain; NOTES.txt has the cleanup line.
*/ -}}
{{- define "mto-extension-argocd.retainAnnotations" -}}
helm.sh/resource-policy: keep
argocd.argoproj.io/sync-options: Prune=false,Delete=false
{{- end -}}

{{- define "mto-extension-argocd.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- /*
Each sourceRepos.pattern entry is expanded per tenant by substituting the literal
`{{ tenant }}` (or `{{tenant}}`). Any other placeholder form -- `{{ .tenant }}`,
`{{ .Tenant }}`, `${tenant}`, `$(tenant)`, `%s` -- is not substituted, and the
placeholder would ship verbatim into a live AppProject's sourceRepos. That
grants access to a repository that cannot exist, silently, and it looks right
at a glance. So the check is not just for leftover braces: every common
placeholder syntax is rejected. (`%s` is safe to reject outright -- `s` is not
a hex digit, so it can never be legitimate percent-encoding in a repo URL.)

Caught here, at helm install time, rather than in the parent gotemplate: the
patterns are chart-wide values, so a bad one is a global misconfiguration, and
refusing the install is both the earliest and the safest place to say so.

The list shape is checked here too, because the schema is skippable: a plain
string (the chart's earlier shape) or an empty entry would otherwise reach
every tenant's AppProject.

So are duplicates, with `{{tenant}}` and `{{ tenant }}` read as the same
placeholder. ArgoCD's API refuses a project that lists a repo twice. The child
gotemplate drops duplicates as well, so this check only makes the mistake
visible at install time.
*/ -}}
{{- define "mto-extension-argocd.validateSourceReposPattern" -}}
{{- $ps := (.Values.argocd.sourceRepos).pattern | default list -}}
{{- if not (kindIs "slice" $ps) -}}
{{- fail (printf "argocd.sourceRepos.pattern must be a list of strings, got %s %q; write it as a list, for example pattern: [%q]" (kindOf $ps) (toString $ps) (toString $ps)) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $p := $ps -}}
{{- if not (kindIs "string" $p) -}}
{{- fail (printf "argocd.sourceRepos.pattern entries must be strings, got %s (%v)" (kindOf $p) $p) -}}
{{- end -}}
{{- if eq $p "" -}}
{{- fail "argocd.sourceRepos.pattern contains an empty entry, which would ship an empty repository into every tenant's AppProject" -}}
{{- end -}}
{{- $norm := replace "{{tenant}}" "{{ tenant }}" $p -}}
{{- if hasKey $seen $norm -}}
{{- fail (printf "argocd.sourceRepos.pattern lists %q twice ({{tenant}} and {{ tenant }} count as the same); ArgoCD refuses a project with a duplicate repository" $norm) -}}
{{- end -}}
{{- $_ := set $seen $norm true -}}
{{- $expanded := replace "{{tenant}}" "x" (replace "{{ tenant }}" "x" $p) -}}
{{- range $residue := list "{{" "${" "$(" "%s" -}}
{{- if contains $residue $expanded -}}
{{- fail (printf "argocd.sourceRepos.pattern entry %q contains an unrecognised placeholder (%q); only {{ tenant }} and {{tenant}} are substituted" $p $residue) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
Same skippable-schema reasoning as the guards around it: values.schema.json
enforces minLength 1, but the schema is skippable. A blank argocd.namespace
does NOT render zero children -- it renders one per tenant, each emitting an
AppProject with an empty namespace that the API server rejects, so every
tenant breaks at once, and the error surfaces later, per tenant, on cluster.
Nil-safe access because this must also hold under `argocd: null`.
*/ -}}
{{- define "mto-extension-argocd.validateArgoNamespace" -}}
{{- $ns := (.Values.argocd).namespace -}}
{{- if not $ns -}}
{{- fail "argocd.namespace is required. A blank value renders one child per tenant, each emitting an AppProject with an empty namespace the API server rejects -- every tenant breaks at once." -}}
{{- end -}}
{{- /* A malformed name passes minLength but the API server rejects every
       rendered AppProject on cluster, late. Enforce DNS-1123 at install time. */ -}}
{{- if or (gt (len $ns) 63) (not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $ns)) -}}
{{- fail (printf "argocd.namespace %q is not a valid namespace name (DNS-1123 label: lowercase alphanumerics and '-', bounded by alphanumerics, <=63 chars). The API server would reject every rendered AppProject." $ns) -}}
{{- end -}}
{{- end -}}

{{- /*
An invalid label/annotation key is the quietest misconfiguration: the
gotemplates' hasKey/dig lookups never match it, so the override or opt-in it
names is silently ignored. Enforce the Kubernetes qualified-name format at
install time. Called with (dict "field" <name> "value" <key>).
*/ -}}
{{- define "mto-extension-argocd.assertLabelKey" -}}
{{- $v := .value -}}
{{- if not (regexMatch `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$` $v) -}}
{{- fail (printf "%s %q is not a valid Kubernetes label/annotation key (an optional dns-subdomain/ prefix, then alphanumerics and -_. bounded by alphanumerics)" .field $v) -}}
{{- end -}}
{{- end -}}

{{- /*
Every key the gotemplates read off a Tenant or a Namespace must be non-empty.
The schema enforces minLength 1 on all of them, but the schema is skippable --
and the failure mode here is the quietest in the chart: an empty key makes
`dig`/`hasKey` match nothing, so every per-tenant override (including
on-delete-purge, which decides whether an AppProject survives deletion) is
silently ignored and the chart default silently wins. Nothing ever errors.
An empty tenantNamespaceLabel is the same shape: no namespace ever matches,
and every tenant renders with no destinations except the ArgoCD namespace.
*/ -}}
{{- define "mto-extension-argocd.validateOverrideKeys" -}}
{{- $a := .Values.annotations | default dict -}}
{{- range $k := list "sourceRepos" "clusterResourceWhitelist" "namespaceResourceBlacklist" "onDeletePurge" -}}
{{- if not (get $a $k) -}}
{{- fail (printf "annotations.%s must be a non-empty annotation key; an empty key never matches any Tenant annotation, so that per-tenant override is silently ignored and the chart default silently wins" $k) -}}
{{- end -}}
{{- include "mto-extension-argocd.assertLabelKey" (dict "field" (printf "annotations.%s" $k) "value" (get $a $k)) -}}
{{- end -}}
{{- $s := .Values.scaffolding | default dict -}}
{{- if not (default dict $s.annotations).enabled -}}
{{- fail "scaffolding.annotations.enabled must be a non-empty annotation key; in OnAnnotation mode an empty key matches no Tenant, so zero children are emitted and orphan reaping then deletes or releases every AppProject this release manages" -}}
{{- end -}}
{{- include "mto-extension-argocd.assertLabelKey" (dict "field" "scaffolding.annotations.enabled" "value" (default dict $s.annotations).enabled) -}}
{{- if not (default dict $s.labels).enabled -}}
{{- fail "scaffolding.labels.enabled must be a non-empty label key; in OnLabel mode an empty key matches no Tenant, so zero children are emitted and orphan reaping then deletes or releases every AppProject this release manages" -}}
{{- end -}}
{{- include "mto-extension-argocd.assertLabelKey" (dict "field" "scaffolding.labels.enabled" "value" (default dict $s.labels).enabled) -}}
{{- if not .Values.tenantNamespaceLabel -}}
{{- fail "tenantNamespaceLabel must be a non-empty label key; an empty key matches no Namespace, so every tenant's AppProject silently loses every destination except the ArgoCD namespace" -}}
{{- end -}}
{{- include "mto-extension-argocd.assertLabelKey" (dict "field" "tenantNamespaceLabel" "value" .Values.tenantNamespaceLabel) -}}
{{- end -}}

{{- /*
The four annotations.* override keys and scaffolding.annotations.enabled all
read from the SAME map -- the Tenant's annotations -- so a shared key makes the
opt-in marker and an override read each other's value, silently. Scoped to
annotation keys: the label keys live in other maps and cannot collide here.
*/ -}}
{{- define "mto-extension-argocd.validateKeyCollisions" -}}
{{- $a := .Values.annotations | default dict -}}
{{- $s := .Values.scaffolding | default dict -}}
{{- $keys := list $a.sourceRepos $a.clusterResourceWhitelist $a.namespaceResourceBlacklist $a.onDeletePurge (default dict $s.annotations).enabled -}}
{{- if ne (len $keys) (len (uniq $keys)) -}}
{{- fail (printf "annotation keys collide: the four annotations.* keys and scaffolding.annotations.enabled must all be distinct, got %v. A shared key makes the opt-in marker and an override read each other's value on the same Tenant." $keys) -}}
{{- end -}}
{{- end -}}

{{- /*
onDeletePurgeAppProject is otherwise the schema's job (required + boolean),
but the schema is skippable, and helm strips explicitly-null keys before
validation -- so on a skip-schema path a null reaches the templates and
renders "<nil>" into every child's on-delete-purge parameter. Every tenant
fails at once, and the child's error message blames the tenant's annotation
for what is a chart value.
*/ -}}
{{- define "mto-extension-argocd.validateOnDeletePurge" -}}
{{- if not (kindIs "bool" .Values.onDeletePurgeAppProject) -}}
{{- fail (printf "onDeletePurgeAppProject must be true or false (a boolean), got %s. Anything else renders \"<nil>\" into every child's on-delete-purge parameter and fails every tenant at once." (kindOf .Values.onDeletePurgeAppProject)) -}}
{{- end -}}
{{- end -}}

{{- /*
The two default lists render into every child via toJson. The schema types
them, but on a skip-schema path a scalar here renders defaultWhitelistJson as
a JSON string, the child's mustFromJson yields a string, and range then dies
per tenant on cluster with a type error -- a chart-wide misconfiguration
failing late, per tenant, and obscurely. Refused at install time like every
other chart-wide value. Absent or null is fine: `| default list` turns it
into [].
*/ -}}
{{- define "mto-extension-argocd.validateDefaultLists" -}}
{{- $a := .Values.argocd | default dict -}}
{{- range $k := list "clusterResourceWhitelist" "namespaceResourceBlacklist" -}}
{{- if hasKey $a $k -}}
{{- $v := index $a $k -}}
{{- if and (not (kindIs "invalid" $v)) (not (kindIs "slice" $v)) -}}
{{- fail (printf "argocd.%s must be a list of {group, kind} entries, got %s" $k (kindOf $v)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
Security-relevant values (#6, #7). The schema types them, but it is skippable,
and a template reads the string "false" as true -- which would grant exec,
cluster writes or the control-plane destination. An unknown profile must not
fall back to either role shape silently.
*/ -}}
{{- define "mto-extension-argocd.validateSecurityValues" -}}
{{- $roles := .Values.roles | default dict -}}
{{- $profile := $roles.profile | default "leastPrivilege" | toString -}}
{{- if not (has $profile (list "leastPrivilege" "extensionsCompatible")) -}}
{{- fail (printf "roles.profile must be one of leastPrivilege, extensionsCompatible -- got %q" $profile) -}}
{{- end -}}
{{- range $k := list "exec" "clusters" -}}
{{- $v := index ((index $roles $k) | default dict) "enabled" -}}
{{- if and (not (kindIs "invalid" $v)) (not (kindIs "bool" $v)) -}}
{{- fail (printf "roles.%s.enabled must be true or false (a boolean), got %s %v" $k (kindOf $v) $v) -}}
{{- end -}}
{{- end -}}
{{- $cp := index (.Values.argocd | default dict) "allowControlPlaneDestination" -}}
{{- if and (not (kindIs "invalid" $cp)) (not (kindIs "bool" $cp)) -}}
{{- fail (printf "argocd.allowControlPlaneDestination must be true or false (a boolean), got %s %v" (kindOf $cp) $cp) -}}
{{- end -}}
{{- end -}}

{{- /*
The same reasoning as the sync.mode guard below: values.schema.json already has
an enum, but the schema is skippable (helm >= 3.16 --skip-schema-validation) and
is not applied on every path that renders a chart through the Helm SDK.

The blast radius here is larger than sync.mode's. An unrecognised mode matches
no branch in the parent gotemplate, every tenant fails the scaffolding filter,
zero children are emitted -- and template-operator-v2 reaps orphans, so a typo
takes every AppProject the chart manages out of management: DELETED on
template-operator-v2 v0.0.6 and earlier, released on v0.0.7 and later with
purge off.
*/ -}}
{{- define "mto-extension-argocd.validateScaffoldingMode" -}}
{{- $mode := (.Values.scaffolding).mode | toString -}}
{{- if not (has $mode (list "Always" "OnLabel" "OnAnnotation" "Never")) -}}
{{- fail (printf "scaffolding.mode must be one of Always, OnLabel, OnAnnotation, Never -- got %q. An unrecognised value emits no children, and orphan reaping then deletes or releases every AppProject this release manages." $mode) -}}
{{- end -}}
{{- end -}}

{{- /*
Drift protection, emitted onto both Templates. `off` emits nothing at all
rather than `mode: off`, so the CRD default applies.

The values.schema.json enum already rejects a bad mode, but the schema is
skippable (helm >= 3.16 --skip-schema-validation) and is not applied on every
path that renders a chart through the Helm SDK -- so the check is repeated
here. Note that unquoted `off` parses as a YAML boolean, hence the toString.
*/ -}}
{{- define "mto-extension-argocd.sync" -}}
{{- $sync := .Values.sync | default dict -}}
{{- /* No fallback mode on purpose. A missing sync.mode is only reachable when
       the schema is skipped, and an earlier draft defaulted it to "revert" --
       silently contradicting the shipped `strict` default on exactly the path
       the schema cannot protect. "" fails the enum check below instead. */ -}}
{{- $mode := "" -}}
{{- if hasKey $sync "mode" -}}
{{- $mode = $sync.mode | toString -}}
{{- end -}}
{{- if not (has $mode (list "off" "revert" "strict")) -}}
{{- fail (printf "sync.mode must be one of off, revert, strict — got %q (unquoted off/on/yes/true parse as YAML booleans; quote the value)" $mode) -}}
{{- end -}}
{{- /*
Under strict, the admission webhook refuses any UPDATE that touches an
enforced field. template-operator-v2 manages its own finalizer on every
rendered resource through exactly such an update, so without a
metadata.finalizers exemption it locks itself out and nothing can be torn
down. Version support for strict is stated once, in Chart.yaml.
*/ -}}
{{- if eq $mode "strict" -}}
{{- $ignored := $sync.ignoreFields | default list -}}
{{- if not (has "metadata.finalizers" $ignored) -}}
{{- fail "sync.mode=strict requires \"metadata.finalizers\" in sync.ignoreFields, or template-operator-v2 cannot manage its own finalizer and teardown deadlocks" -}}
{{- end -}}
{{- if not (has "spec.roles[*].jwtTokens" $ignored) -}}
{{- fail "sync.mode=strict requires \"spec.roles[*].jwtTokens\" in sync.ignoreFields, or the webhook denies `argocd proj role create-token` -- ArgoCD writes tokens into the AppProject spec, not status" -}}
{{- end -}}
{{- end -}}
{{- if ne $mode "off" -}}
sync:
  mode: {{ $mode }}
{{- with $sync.ignoreFields }}
  ignoreFields:
    {{- toYaml . | nindent 4 }}
{{- end }}
{{- end -}}
{{- end -}}
