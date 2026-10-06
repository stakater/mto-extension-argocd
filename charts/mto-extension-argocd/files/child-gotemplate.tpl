{{- /*
Renders ONE tenant's AppProject. Executed by template-operator-v2, not by Helm
-- the surrounding chart embeds this file verbatim via .Files.Get, so nothing
here is a Helm template.

Matches ExtensionsArgoCDReconciler's AppProject for the captured tenants in
tests/render/testdata/equivalence, except for the deliberate differences that
README.md lists under "Differences from the Extensions controller".
TestEquivalenceWithExtensionsController asserts each one.

Every list-shaped parameter arrives JSON-encoded. That is mandatory, not
stylistic: TemplateInstance.spec.parameters[].value is a string, and a
comma-joined empty list is "", which ResolveParameterValue treats as unset and
rejects. A tenant with zero namespaces is the common case that breaks.

fromJsonArray does not exist in template-operator-v2, and fromYaml is Helm-only.

Decoding is mustFromJson throughout, never the plain form, for two reasons.

Correctness: malformed JSON must fail this child rather than fall back to the
chart default. Applies are server-side with ForceOwnership, so a silent
fallback would strip the field from a live AppProject. Failing here fails one
tenant -- the parent never parses these, so one malformed annotation cannot
break the others. The plain form swallows the error and yields an error map,
which would render as garbage into a live object.

Testability: the plain form is one of the places Helm and template-operator-v2
genuinely differ. It decodes a top-level array in the operator and refuses one
in Helm, so a gotemplate using it cannot be exercised by the render tests at
all. mustFromJson behaves identically in both, which is what lets the tests
execute this file. TestGotemplatesNeverUsePlainFromJson enforces the rule.
*/ -}}
{{- $t := .parameters.tenant }}
{{- $argoNs := .parameters.argoNamespace }}
{{- $nsList := mustFromJson .parameters.namespacesJson }}
{{- $wl := mustFromJson .parameters.whitelistJson }}
{{- $bl := mustFromJson .parameters.blacklistJson }}
{{- $repos := mustFromJson .parameters.sourceReposJson }}
{{- $priv := mustFromJson .parameters.privilegedGroupsJson }}
{{- $tg := mustFromJson .parameters.tenantGroupsJson }}
{{- $tu := mustFromJson .parameters.tenantUsersJson }}
{{- /* mustFromJson validates JSON syntax, not shape. A wrong-typed value like
       [{"group":{}}] or ["a",123] would stringify via quote ("map[]", "123")
       and apply cleanly to the live object -- silent corruption. Reject a
       present-but-non-string group/kind or repo here so it fails this one child
       instead. An absent group/kind is nil and renders "" (core group) -- kept.
       group and kind are read with `index`, not as fields: from v0.0.8
       template-operator-v2 renders with missingkey=error, and a field read of
       an absent key would fail the child instead of rendering the core group. */ -}}
{{- range $i := concat $wl $bl }}
{{- if and (hasKey $i "group") (not (kindIs "string" (index $i "group"))) }}{{- fail (printf "clusterResourceWhitelist/namespaceResourceBlacklist group must be a string, got %s (%v)" (kindOf (index $i "group")) (index $i "group")) }}{{- end }}
{{- if and (hasKey $i "kind") (not (kindIs "string" (index $i "kind"))) }}{{- fail (printf "clusterResourceWhitelist/namespaceResourceBlacklist kind must be a string, got %s (%v)" (kindOf (index $i "kind")) (index $i "kind")) }}{{- end }}
{{- end }}
{{- if and $repos (not (kindIs "slice" $repos)) }}{{- fail (printf "sourceRepos must be a JSON list, got %s" (kindOf $repos)) }}{{- end }}
{{- range $r := $repos }}
{{- if not (kindIs "string" $r) }}{{- fail (printf "sourceRepos entries must be strings, got %s (%v)" (kindOf $r) $r) }}{{- end }}
{{- end }}
{{- /* Duplicates are dropped, first occurrence kept. ArgoCD's API refuses a
       project that lists a repo twice ("source repository ... already
       added"), so one duplicate would make every write through ArgoCD fail
       -- create-token included. Same for groups below. */ -}}
{{- if $repos }}{{- $repos = mustUniq $repos }}{{- end }}
{{- /* Reject a near-miss on the purge annotation instead of reading it as
       "retain". "True", "yes", "1" and "on" all look affirmative and would
       silently mean the opposite of what the author intended -- and this one
       decides whether a tenant's AppProject survives deletion. Failing here
       fails one child, which is visible; the value came from that tenant's own
       annotation, so nobody else is affected. */ -}}
{{- if not (has .parameters.onDeletePurge (list "true" "false")) }}
{{- fail (printf "on-delete-purge must be exactly \"true\" or \"false\", got %q" .parameters.onDeletePurge) }}
{{- end }}
{{- $purge := eq .parameters.onDeletePurge "true" }}
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: {{ $t | quote }}
  namespace: {{ $argoNs | quote }}
  {{- if not $purge }}
  annotations:
    templates.v2.stakater.com/deletion-policy: Orphan
  {{- end }}
spec:
  description: Managed by mto-extension-argocd
  {{- if $repos }}
  sourceRepos:
  {{- range $r := $repos }}
    - {{ $r | quote }}
  {{- end }}
  {{- end }}
  destinations:
  {{- range $ns := $nsList }}
    - namespace: {{ $ns | quote }}
      server: https://kubernetes.default.svc
  {{- end }}
  {{- /* The ArgoCD namespace is always a destination, even when it matches a
         privileged pattern -- ArgoCD itself needs it to manage the project.

         Guarded against listing it twice: if the ArgoCD namespace also carries
         the tenant label and is not privileged, the parent already put it in
         namespacesJson. */}}
  {{- if not (has $argoNs $nsList) }}
    - namespace: {{ $argoNs | quote }}
      server: https://kubernetes.default.svc
  {{- end }}
  {{- if $wl }}
  clusterResourceWhitelist:
  {{- range $i := $wl }}
    - group: {{ (index $i "group") | quote }}
      kind: {{ (index $i "kind") | quote }}
  {{- end }}
  {{- end }}
  {{- if $bl }}
  namespaceResourceBlacklist:
  {{- range $i := $bl }}
    - group: {{ (index $i "group") | quote }}
      kind: {{ (index $i "kind") | quote }}
  {{- end }}
  {{- end }}
  roles:
  {{- range $role := list "owner" "edit" "view" }}
  {{- /* Go's hex(sha256("<tenant>-<role>"))[:6]. Reproduced exactly by sprig. */}}
  {{- $hash := sha256sum (printf "%s-%s" $t $role) | trunc 6 }}
  {{- $verb := ternary "get" "*" (eq $role "view") }}
    - name: {{ printf "%s-%s" $t $role | quote }}
      description: >-
        Role that gives {{ if eq $role "owner" }}full{{ else }}{{ $role }}{{ end }} access to all
        resources inside the tenant's namespace to the tenant owner group
      {{- /* One line per project-scoped resource, not `*`. ArgoCD's API
             validation refuses `*` as the resource of a project role policy,
             so with `*` every write through the ArgoCD API -- create-token,
             delete-token, the UI -- fails with "project resource must be:
             ...". At runtime `*` and these six lines grant the same access:
             verified on ArgoCD 3.4.6 with can-i over every resource and
             action, for an owner and a viewer. */}}
      policies:
      {{- range $res := list "applications" "applicationsets" "logs" "exec" "repositories" "clusters" }}
        - {{ printf "p, proj:%s:%s-%s, %s, %s, %s/*, allow" $t $t $role $res $verb $t | quote }}
      {{- end }}
      {{- /* Order is load-bearing for the equivalence diff: generated group,
             then the IntegrationConfig's privileged groups, then the tenant's
             own groups, then its users.

             The users are a deliberate addition. The generated group only
             resolves on OpenShift -- syncOCPGroups runs under
             PlatformTypeOpenshift only -- so on vanilla Kubernetes a tenant
             whose owners are users depends on these entries. ArgoCD matches
             them only when argocd-rbac-cm `scopes` includes the claim that
             carries the user name, usually `email` (README, "Before
             production"); with the default [groups] they grant nothing. The
             generated group is kept as well: a chart cannot detect the
             platform, and keeping it is purely additive, so no OpenShift
             tenant loses access. */}}
      {{- /* EVERY value below is quoted, and that is load-bearing, not style.
             These strings come from the Tenant and the IntegrationConfig, which
             tenant authors control. Unquoted, a group named
             "ok\n        - injected-admin-group" renders as TWO list entries
             and silently grants a group nobody configured; a payload with a
             document separator can emit a whole second AppProject, which the
             chart's RBAC permits and template-operator-v2 will apply. Benign
             values break too -- an unquoted `true` becomes a boolean and
             `12345` an int, neither of which ArgoCD accepts as a group. */}}
      {{- /* Duplicates are dropped, first occurrence kept, so the order
             above holds. ArgoCD's API refuses a role that lists a group twice
             ("group ... already exists for role"), and a privileged group
             that a tenant also lists is ordinary configuration. */}}
      {{- $groups := concat (list (printf "%s-%s-group-%s" $t $role $hash)) ($priv | default list) (index $tg $role | default list) (index $tu $role | default list) | mustUniq }}
      groups:
      {{- range $g := $groups }}
        - {{ $g | quote }}
      {{- end }}
  {{- end }}
