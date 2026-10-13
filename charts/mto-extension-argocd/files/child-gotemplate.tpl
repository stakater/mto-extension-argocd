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
{{- /* Chart-wide and parent-set values; each must be exactly what the chart
       renders. A near-miss fails this child rather than silently granting. */ -}}
{{- range $p := list "controlPlaneDestination" "rolesExec" "rolesClusters" "impersonation" }}
{{- if not (has (index $.parameters $p) (list "true" "false")) }}
{{- fail (printf "%s must be exactly \"true\" or \"false\", got %q" $p (index $.parameters $p)) }}
{{- end }}
{{- end }}
{{- if not (has .parameters.rolesProfile (list "leastPrivilege" "extensionsCompatible")) }}
{{- fail (printf "rolesProfile must be leastPrivilege or extensionsCompatible, got %q" .parameters.rolesProfile) }}
{{- end }}
{{- $cp := eq .parameters.controlPlaneDestination "true" }}
{{- $impersonate := eq .parameters.impersonation "true" }}
{{- if not (has .parameters.identityMode (list "accessControl" "groupPattern" "both")) }}
{{- fail (printf "identityMode must be accessControl, groupPattern or both, got %q" .parameters.identityMode) }}
{{- end }}
{{- $useACL := ne .parameters.identityMode "groupPattern" }}
{{- $usePattern := ne .parameters.identityMode "accessControl" }}
{{- $sns := mustFromJson .parameters.sourceNamespacesJson }}
{{- /* #12. Every window is checked field by field and rendered field by field:
       a value that would apply cleanly but mean something else must fail
       this tenant instead. */ -}}
{{- if not (has .parameters.orphanedResources (list "off" "warn" "monitor")) }}
{{- fail (printf "orphaned-resources must be off, warn or monitor, got %q" .parameters.orphanedResources) }}
{{- end }}
{{- $sw := mustFromJson .parameters.syncWindowsJson }}
{{- if not (kindIs "slice" $sw) }}{{- fail (printf "sync-windows must be a JSON list, got %s" (kindOf $sw)) }}{{- end }}
{{- range $w := $sw }}
{{- if not (kindIs "map" $w) }}{{- fail (printf "sync-windows entries must be objects, got %s" (kindOf $w)) }}{{- end }}
{{- range $k, $_ := $w }}{{- if not (has $k (list "kind" "schedule" "duration" "applications" "namespaces" "clusters" "manualSync" "timeZone" "andOperator" "description" "syncOverrun")) }}{{- fail (printf "sync-windows: unknown field %q" $k) }}{{- end }}{{- end }}
{{- if not (has (index $w "kind") (list "allow" "deny")) }}{{- fail (printf "sync-windows: kind must be allow or deny, got %v" (index $w "kind")) }}{{- end }}
{{- range $k := list "schedule" "duration" }}{{- if or (not (kindIs "string" (index $w $k))) (eq (index $w $k) "") }}{{- fail (printf "sync-windows: %s must be a non-empty string" $k) }}{{- end }}{{- end }}
{{- /* ArgoCD parses these only when it evaluates the window, and a bad one
       blocks every sync in the project ("invalid sync windows"). Its parser
       takes exactly five cron fields, no @daily, and a Go duration. */ -}}
{{- if not (regexMatch "^\\s*[0-9A-Za-z*?/,-]+(\\s+[0-9A-Za-z*?/,-]+){4}\\s*$" (index $w "schedule")) }}{{- fail (printf "sync-windows: schedule %q must be a cron expression with five fields: minute hour day-of-month month day-of-week" (index $w "schedule")) }}{{- end }}
{{- if not (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$" (index $w "duration")) }}{{- fail (printf "sync-windows: duration %q must be a Go duration, such as 8h or 1h30m" (index $w "duration")) }}{{- end }}
{{- range $k := list "timeZone" "description" }}{{- if and (hasKey $w $k) (not (kindIs "string" (index $w $k))) }}{{- fail (printf "sync-windows: %s must be a string" $k) }}{{- end }}{{- end }}
{{- range $k := list "manualSync" "andOperator" "syncOverrun" }}{{- if and (hasKey $w $k) (not (kindIs "bool" (index $w $k))) }}{{- fail (printf "sync-windows: %s must be true or false" $k) }}{{- end }}{{- end }}
{{- range $k := list "applications" "namespaces" "clusters" }}{{- if hasKey $w $k }}{{- if not (kindIs "slice" (index $w $k)) }}{{- fail (printf "sync-windows: %s must be a list" $k) }}{{- end }}{{- range $s := index $w $k }}{{- if not (kindIs "string" $s) }}{{- fail (printf "sync-windows: %s entries must be strings" $k) }}{{- end }}{{- end }}{{- end }}{{- end }}
{{- end }}
{{- range $r := mustFromJson .parameters.sourceNamespacesRefusedJson }}{{- fail $r }}{{- end }}
{{- $legacyRoles := eq .parameters.rolesProfile "extensionsCompatible" }}
{{- $exec := eq .parameters.rolesExec "true" }}
{{- $clusters := eq .parameters.rolesClusters "true" }}
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
  {{- /* The ArgoCD namespace is NOT a destination unless the platform opts in
         (#6). It does not need to be one: the AppProject lives there whatever
         `destinations` says, and destinations only limit where Applications
         deploy. As a destination it is a path to ArgoCD admin -- an
         Application in the control-plane namespace may reference any
         AppProject, and the controller syncing into it is cluster-admin.

         With the opt-in it is the legacy shape exactly: appended even when it
         matches a privileged pattern, and guarded against listing it twice
         when the parent already put it in namespacesJson (labelled for this
         tenant and not privileged). */}}
  {{- if $sns }}
  sourceNamespaces:
  {{- range $s := $sns }}
    - {{ $s | quote }}
  {{- end }}
  {{- end }}
  {{- $dests := $nsList }}
  {{- if and $cp (not (has $argoNs $nsList)) }}{{- $dests = append $nsList $argoNs }}{{- end }}
  {{- if $dests }}
  destinations:
  {{- range $ns := $dests }}
    - namespace: {{ $ns | quote }}
      server: https://kubernetes.default.svc
  {{- end }}
  {{- else }}
  destinations: []
  {{- end }}
  {{- /* #15. One entry per tenant namespace -- $nsList, not $dests, so the
         ArgoCD namespace never gets one even when it is a destination: there
         is no tenant deployer in it. No catch-all: an unmatched destination
         must fail the sync rather than fall back to the controller. */}}
  {{- if and $impersonate $nsList }}
  destinationServiceAccounts:
  {{- range $ns := $nsList }}
    - server: https://kubernetes.default.svc
      namespace: {{ $ns | quote }}
      defaultServiceAccount: {{ $.parameters.impersonationServiceAccount | quote }}
  {{- end }}
  {{- end }}
  {{- if $sw }}
  syncWindows:
  {{- range $w := $sw }}
    - kind: {{ index $w "kind" | quote }}
      schedule: {{ index $w "schedule" | quote }}
      duration: {{ index $w "duration" | quote }}
      {{- range $k := list "applications" "namespaces" "clusters" }}
      {{- if hasKey $w $k }}
      {{ $k }}:
      {{- range $s := index $w $k }}
        - {{ $s | quote }}
      {{- end }}
      {{- end }}
      {{- end }}
      {{- range $k := list "manualSync" "andOperator" "syncOverrun" }}
      {{- if hasKey $w $k }}
      {{ $k }}: {{ index $w $k }}
      {{- end }}
      {{- end }}
      {{- range $k := list "timeZone" "description" }}
      {{- if hasKey $w $k }}
      {{ $k }}: {{ index $w $k | quote }}
      {{- end }}
      {{- end }}
  {{- end }}
  {{- end }}
  {{- if ne .parameters.orphanedResources "off" }}
  orphanedResources:
    warn: {{ eq .parameters.orphanedResources "warn" }}
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
  {{- /* Policy rules as resource/action pairs. #7: least privilege by default;
         the legacy six-line `*`/`get` shape only with extensionsCompatible. */}}
  {{- $rules := list }}
  {{- if $legacyRoles }}
  {{- $verb := ternary "get" "*" (eq $role "view") }}
  {{- range $res := list "applications" "applicationsets" "logs" "exec" "repositories" "clusters" }}
  {{- $rules = append $rules (list $res $verb) }}
  {{- end }}
  {{- else if eq $role "owner" }}
  {{- $rules = list (list "applications" "*") (list "applicationsets" "*") (list "logs" "get") }}
  {{- if $exec }}{{- $rules = append $rules (list "exec" "create") }}{{- end }}
  {{- $rules = append $rules (list "repositories" "*") }}
  {{- $rules = append $rules (list "clusters" (ternary "*" "get" $clusters)) }}
  {{- else if eq $role "edit" }}
  {{- /* No create, delete or override: edit syncs and updates what exists. */}}
  {{- $rules = list (list "applications" "get") (list "applications" "sync") (list "applications" "update") (list "applications" "update/*") (list "applications" "action/*") (list "applicationsets" "get") (list "logs" "get") (list "repositories" "get") (list "clusters" "get") }}
  {{- else }}
  {{- $rules = list (list "applications" "get") (list "applicationsets" "get") (list "logs" "get") (list "repositories" "get") (list "clusters" "get") }}
  {{- end }}
    - name: {{ printf "%s-%s" $t $role | quote }}
      description: >-
        Role that gives {{ if eq $role "owner" }}full{{ else }}{{ $role }}{{ end }} access to all
        resources inside the tenant's namespace to the tenant owner group
      {{- /* One line per resource/action, never `*` as the resource: ArgoCD's
             API refuses it in a project role policy, so every write through
             the ArgoCD API -- create-token, delete-token, the UI -- would fail
             with "project resource must be: ...". */}}
      policies:
      {{- range $r := $rules }}
        - {{ printf "p, proj:%s:%s-%s, %s, %s, %s/*, allow" $t $t $role (index $r 0) (index $r 1) $t | quote }}
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
      {{- /* #10: the Tenant's lists, the pattern group, or both -- the
             generated and privileged groups lead in every mode. */}}
      {{- $own := list }}
      {{- if $useACL }}{{- $own = concat (index $tg $role | default list) (index $tu $role | default list) }}{{- end }}
      {{- if $usePattern }}
      {{- $tier := index (dict "owner" "owners" "edit" "editors" "view" "viewers") $role }}
      {{- $pg := $.parameters.identityGroupPattern | replace "{{tenant}}" $t | replace "{{ tenant }}" $t | replace "{{tier}}" $tier | replace "{{ tier }}" $tier }}
      {{- $own = append $own $pg }}
      {{- end }}
      {{- $groups := concat (list (printf "%s-%s-group-%s" $t $role $hash)) ($priv | default list) $own | mustUniq }}
      groups:
      {{- range $g := $groups }}
        - {{ $g | quote }}
      {{- end }}
  {{- end }}
