{{- /*
Renders ONE tenant's AppProject. Executed by template-operator-v2, not by Helm
-- the surrounding chart embeds this file verbatim via .Files.Get, so nothing
here is a Helm template.

Ported from the live-cluster spike, whose manifests produced an
AppProject byte-identical to ExtensionsArgoCDReconciler's on a live cluster.
Two deliberate deltas since: the description, and roles[].groups gaining the
tenant's users.

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
       instead. An absent group/kind is nil and renders "" (core group) -- kept. */ -}}
{{- range $i := concat $wl $bl }}
{{- if and (hasKey $i "group") (not (kindIs "string" $i.group)) }}{{- fail (printf "clusterResourceWhitelist/namespaceResourceBlacklist group must be a string, got %s (%v)" (kindOf $i.group) $i.group) }}{{- end }}
{{- if and (hasKey $i "kind") (not (kindIs "string" $i.kind)) }}{{- fail (printf "clusterResourceWhitelist/namespaceResourceBlacklist kind must be a string, got %s (%v)" (kindOf $i.kind) $i.kind) }}{{- end }}
{{- end }}
{{- range $r := $repos }}
{{- if not (kindIs "string" $r) }}{{- fail (printf "sourceRepos entries must be strings, got %s (%v)" (kindOf $r) $r) }}{{- end }}
{{- end }}
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
    - group: {{ $i.group | quote }}
      kind: {{ $i.kind | quote }}
  {{- end }}
  {{- end }}
  {{- if $bl }}
  namespaceResourceBlacklist:
  {{- range $i := $bl }}
    - group: {{ $i.group | quote }}
      kind: {{ $i.kind | quote }}
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
      policies:
        - {{ printf "p, proj:%s:%s-%s, *, %s, %s/*, allow" $t $t $role $verb $t | quote }}
      {{- /* Order is load-bearing for the equivalence diff: generated group,
             then the IntegrationConfig's privileged groups, then the tenant's
             own groups, then its users.

             The users are the one deliberate addition. The generated group only
             resolves on OpenShift -- syncOCPGroups runs under
             PlatformTypeOpenshift only -- so on vanilla Kubernetes a tenant
             would otherwise get no ArgoCD access at all. The generated group is
             kept as well: a chart cannot detect the platform, and keeping it is
             purely additive, so no OpenShift tenant loses access. */}}
      {{- /* EVERY value below is quoted, and that is load-bearing, not style.
             These strings come from the Tenant and the IntegrationConfig, which
             tenant authors control. Unquoted, a group named
             "ok\n        - injected-admin-group" renders as TWO list entries
             and silently grants a group nobody configured; a payload with a
             document separator can emit a whole second AppProject, which the
             chart's RBAC permits and template-operator-v2 will apply. Benign
             values break too -- an unquoted `true` becomes a boolean and
             `12345` an int, neither of which ArgoCD accepts as a group. */}}
      groups:
        - {{ printf "%s-%s-group-%s" $t $role $hash | quote }}
      {{- range $g := $priv }}
        - {{ $g | quote }}
      {{- end }}
      {{- range $g := (index $tg $role) }}
        - {{ $g | quote }}
      {{- end }}
      {{- range $u := (index $tu $role) }}
        - {{ $u | quote }}
      {{- end }}
  {{- end }}
