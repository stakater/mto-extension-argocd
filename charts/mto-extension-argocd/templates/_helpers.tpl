{{- define "mto-extension-argocd.childTemplateName" -}}
mto-argocd-child
{{- end -}}

{{- define "mto-extension-argocd.parentTemplateName" -}}
mto-argocd-parent
{{- end -}}

{{- define "mto-extension-argocd.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- /*
Drift protection, emitted onto both Templates. `off` emits nothing at all
rather than `mode: off`, so the CRD default applies.

The values.schema.json enum already rejects a bad mode, but the schema is
skippable (helm >= 3.16 --skip-schema-validation) and is not applied on every
path that renders a chart through the Helm SDK -- so the check is repeated
here. Note that unquoted `off` parses as a YAML boolean, hence the toString.
*/ -}}
{{- /*
sourceRepos.pattern is expanded per tenant by substituting the literal
`{{ tenant }}` (or `{{tenant}}`). Any other placeholder form -- `{{ .tenant }}`,
`{{ .Tenant }}`, `${tenant}` -- is not substituted, and the braces would ship
verbatim into a live AppProject's sourceRepos. That grants access to a
repository that cannot exist, silently, and it looks right at a glance.

Caught here, at helm install time, rather than in the parent gotemplate: the
pattern is one chart-wide value, so a bad one is a global misconfiguration, and
refusing the install is both the earliest and the safest place to say so.
*/ -}}
{{- define "mto-extension-argocd.validateSourceReposPattern" -}}
{{- $p := (.Values.argocd.sourceRepos).pattern | default "" -}}
{{- if $p -}}
{{- $expanded := replace "{{tenant}}" "x" (replace "{{ tenant }}" "x" $p) -}}
{{- if contains "{{" $expanded -}}
{{- fail (printf "argocd.sourceRepos.pattern %q contains an unrecognised placeholder; only {{ tenant }} and {{tenant}} are substituted" $p) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
The same reasoning as the sync.mode guard below: values.schema.json already has
an enum, but the schema is skippable (helm >= 3.16 --skip-schema-validation) and
is not applied on every path that renders a chart through the Helm SDK.

The blast radius here is larger than sync.mode's. An unrecognised mode matches
no branch in the parent gotemplate, every tenant fails the scaffolding filter,
zero children are emitted -- and template-operator-v2 reaps orphans, so a typo
DELETES every AppProject the chart manages.
*/ -}}
{{- define "mto-extension-argocd.validateScaffoldingMode" -}}
{{- $mode := .Values.scaffolding.mode | toString -}}
{{- if not (has $mode (list "Always" "OnLabel" "OnAnnotation" "Never")) -}}
{{- fail (printf "scaffolding.mode must be one of Always, OnLabel, OnAnnotation, Never -- got %q. An unrecognised value emits no children, and orphan reaping then deletes every AppProject this release manages." $mode) -}}
{{- end -}}
{{- end -}}

{{- define "mto-extension-argocd.sync" -}}
{{- $sync := .Values.sync | default dict -}}
{{- $mode := "revert" -}}
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
down. Verified working with the exemption on tov2 v0.0.5; the webhook has
shipped since v0.0.3.
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
