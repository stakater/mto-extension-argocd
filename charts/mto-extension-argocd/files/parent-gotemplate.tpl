{{- /*
Ranges over every Tenant and emits one child TemplateInstance per tenant that
passes the scaffolding filter. Executed by template-operator-v2, not by Helm.

Three parameters, three watches, independent of tenant count: a cluster-wide
LIST of Tenants, a cluster-wide LIST of Namespaces, and one namespaced GET of
the IntegrationConfig. Verified live -- all three register watches, and a
namespace event affecting one tenant rewrites only that tenant's child.

Namespaces are LISTed cluster-wide and filtered here rather than with a
namespaceSelector. namespaceSelector scopes where to look for a namespaced
kind, is not a filter on Namespace objects, and errors on cluster-scoped
targets. Filtering here also means NamespaceSelectorMatchedZero can never
fire, there is one namespace watch instead of one per tenant, and privileged
filtering happens in exactly one place.
*/ -}}
{{- /* Optional keys are read with `index`, never as fields. From v0.0.8
       template-operator-v2 renders with missingkey=error, so a field read
       like `$t.metadata.labels` on a Tenant without labels fails the whole
       parent -- every tenant at once. `index` is exempt from that option and
       returns nil for a missing key, which is what the field form returns on
       v0.0.7 and earlier, so the output is the same on every version.
       Only metadata and metadata.name, which every API object has, are read
       as fields. */ -}}
{{- $opNs := .parameters.operatorNamespace }}
{{- $childTemplate := .parameters.childTemplateName }}
{{- $argoNs := .parameters.argoNamespace }}
{{- $nsLabel := .parameters.tenantNamespaceLabel }}
{{- $mode := .parameters.scaffoldingMode }}
{{- $annoKey := .parameters.scaffoldingAnnotationKey }}
{{- $labelKey := .parameters.scaffoldingLabelKey }}
{{- $defaultWl := .parameters.defaultWhitelistJson }}
{{- $defaultBl := .parameters.defaultBlacklistJson }}
{{- $reposPatterns := mustFromJson .parameters.sourceReposPatternsJson }}
{{- $defaultPurge := .parameters.defaultOnDeletePurge }}
{{- $aRepos := .parameters.annotationSourceRepos }}
{{- $aWl := .parameters.annotationClusterResourceWhitelist }}
{{- $aBl := .parameters.annotationNamespaceResourceBlacklist }}
{{- $aPurge := .parameters.annotationOnDeletePurge }}
{{- $aSW := .parameters.annotationSyncWindows }}
{{- $aOR := .parameters.annotationOrphanedResources }}
{{- /* #6. Off by default: the ArgoCD namespace is never a tenant destination,
       even when it carries the tenant's label. On, it is the legacy shape. */ -}}
{{- $allowCP := eq .parameters.allowControlPlaneDestination "true" }}
{{- $snOn := eq .parameters.sourceNamespacesEnabled "true" }}
{{- $snPattern := .parameters.sourceNamespacesPattern }}
{{- /*
Read the IntegrationConfig shallowly and dig. The parameter resolves the whole
.spec as a map; a deep jsonPath like .spec.accessControl.privileged fails the
parent outright when the field is absent.

This parameter carries no defaultValue, deliberately -- but that alone is NOT
enough to be fail-closed, and an earlier version of this template got it wrong.
`dig` with a dict default plus `| default list` turns every missing key into an
empty privileged list, which disables filtering entirely and puts privileged
namespaces into every tenant's destinations. The effective fallback was [], the
exact inverse of what was intended.

So the absence of accessControl.privileged.namespaces is an error here, not a
default. MTO's own fallback is [".*"] -- deny everything -- and refusing to
render is the closest a template can get to that.

hasKey alone is not enough either: it is TRUE when the key exists holding an
explicit null, and `| default list` then quietly turns that null into [] --
the same fail-open, one shape over. So null is rejected alongside absence.

Each level is defaulted in turn rather than dug: dig needs a map at every
level and errors on an explicit null with a Go type-assertion message,
instead of reaching the actionable failure below.
*/ -}}
{{- $ic := default dict .parameters.integrationConfig }}
{{- $icAc := default dict (index $ic "accessControl") }}
{{- $priv := default dict (index $icAc "privileged") }}
{{- if or (not (hasKey $priv "namespaces")) (kindIs "invalid" (index $priv "namespaces")) }}
{{- fail "IntegrationConfig .spec.accessControl.privileged.namespaces is absent or null; refusing to render because an empty privileged list would put privileged namespaces into every tenant's destinations. Set it explicitly, to [] if the cluster genuinely has none." }}
{{- end }}
{{- $privNs := index $priv "namespaces" | default list }}
{{- /* An empty-string pattern is a valid empty regex that matches EVERY
       namespace, so one stray "" would strip all destinations from every
       tenant. mustRegexMatch does not catch it. Fail the parent (fail-closed),
       like the absent/null guard above. ".*" is a legitimate deny-all -- kept. */ -}}
{{- range $p := $privNs }}
{{- if eq $p "" }}
{{- fail "IntegrationConfig .spec.accessControl.privileged.namespaces contains an empty pattern, which matches every namespace and would strip all destinations from every tenant. Remove the empty entry (use \".*\" if you truly mean deny-all)." }}
{{- end }}
{{- end }}
{{- $privGroups := index $priv "groups" | default list }}
{{- /* Pass 1: which tenants get an AppProject. No child emitted means no
       AppProject -- and, because template-operator-v2 reaps orphans,
       narrowing this filter removes the AppProjects it excludes: deleted on
       v0.0.6 and earlier, released on v0.0.7 and later with purge off. */ -}}
{{- $taken := dict }}
{{- range $t := .parameters.tenants }}
{{- $labels := default dict (index $t.metadata "labels") }}
{{- $annos := default dict (index $t.metadata "annotations") }}
{{- $take := false }}
{{- if eq $mode "Always" }}{{- $take = true }}
{{- else if eq $mode "OnLabel" }}{{- $take = eq (dig $labelKey "" $labels) "true" }}
{{- else if eq $mode "OnAnnotation" }}{{- $take = eq (dig $annoKey "" $annos) "true" }}
{{- end }}
{{- if $take }}{{- $_ := set $taken $t.metadata.name true }}{{- end }}
{{- end }}
{{- /* Pass 2: every opted-in tenant's namespaces, minus the privileged ones,
       in ONE pass over the Namespace list. Scanning the whole list once per
       tenant costs tenants x namespaces template steps: 1.9 s per render at
       300 tenants and 3000 namespaces, and the parent re-renders on every
       Namespace event in the cluster. LIST order is kept inside each
       tenant's bucket. Skipped when no tenant is opted in, as the per-tenant
       scan was, so the privileged regexes run on exactly the same namespaces
       as before: those of opted-in tenants.

       regexMatch is UNANCHORED, matching Go's regexp.MatchString in the
       controller this replaces. A pattern of "openshift" therefore also
       excludes "my-openshift-tools". That is existing behaviour, reproduced
       on purpose -- and it is why the privileged fixtures are the first test
       in this repo: MTO's managed-prefix webhook makes this path untestable
       on a real cluster, and getting it wrong puts one tenant's namespaces
       into another tenant's AppProject. */ -}}
{{- $byTenant := dict }}
{{- if $taken }}
{{- range $ns := .parameters.namespaces }}
{{- $owner := dig $nsLabel "" (default dict (index $ns.metadata "labels")) }}
{{- if and $owner (hasKey $taken $owner) (or $allowCP (ne $ns.metadata.name $argoNs)) }}
{{- $isPriv := false }}
{{- /* mustRegexMatch, not regexMatch: the plain form discards the compile
       error, so a typo'd pattern in the cluster's IntegrationConfig silently
       stops filtering -- for every tenant at once. */ -}}
{{- range $p := $privNs }}{{- if mustRegexMatch $p $ns.metadata.name }}{{- $isPriv = true }}{{- end }}{{- end }}
{{- if not $isPriv }}{{- $_ := set $byTenant $owner (append (get $byTenant $owner | default list) $ns.metadata.name) }}{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- /* Pass 3: one child per opted-in tenant, in Tenant LIST order. */ -}}
{{- range $t := .parameters.tenants }}
{{- $name := $t.metadata.name }}
{{- if hasKey $taken $name }}
{{- $annos := default dict (index $t.metadata "annotations") }}
{{- $mine := get $byTenant $name | default list }}
{{- /* Two steps, and both are needed. Measured against every shape a Tenant can
       take:

         shape                $t.spec.accessControl   dig "spec" "accessControl"
         no spec key          nil-pointer error       ok
         spec: {}             ok                      ok
         spec: null           nil-pointer error       interface-conversion error
         accessControl: null  ok                      interface-conversion error

       Neither single form survives all four. Any failure here kills the
       PARENT, so one malformed Tenant would take every other tenant's
       AppProject down with it. Defaulting each level in turn is the only form
       that handles every shape. */ -}}
{{- $tspec := default dict (index $t "spec") }}
{{- $ac := default dict (index $tspec "accessControl") }}
{{- /* Each role block is defaulted too. `dig "owners" "groups" list $ac`
       looks safe but errors when accessControl.owners is explicitly null --
       dig needs a map at every level and gets nil. Defaulting each level in
       turn is the only form that survives every shape. */ -}}
{{- $owners := default dict (index $ac "owners") }}
{{- $editors := default dict (index $ac "editors") }}
{{- $viewers := default dict (index $ac "viewers") }}
{{- $tg := dict
      "owner" (default list (index $owners "groups"))
      "edit"  (default list (index $editors "groups"))
      "view"  (default list (index $viewers "groups")) }}
{{- $tu := dict
      "owner" (default list (index $owners "users"))
      "edit"  (default list (index $editors "users"))
      "view"  (default list (index $viewers "users")) }}
{{- /* Per-tenant overrides. An absent annotation inherits the chart default; an
       explicit "[]" overrides with empty. The annotation's value is passed down
       VERBATIM and parsed in the child, so malformed JSON fails one tenant
       instead of the whole parent.

       Present-or-absent is decided with hasKey, not dig-with-default: `default`
       treats "" as unset, so an explicit empty-string annotation would silently
       inherit the chart default -- the near-miss of the "[]" contract, on an
       author who plainly meant to override SOMETHING. Passed through instead,
       "" fails that one child loudly, exactly like malformed JSON.

       A key present with a NIL value cannot come off a live cluster
       (metadata.annotations is map[string]string), only from a hand-written
       test fixture -- where `nil | quote` renders an empty parameter value
       and the render harness fails that child loudly. */ -}}
{{- $wlJson := $defaultWl }}{{- if hasKey $annos $aWl }}{{- $wlJson = index $annos $aWl }}{{- end }}
{{- $blJson := $defaultBl }}{{- if hasKey $annos $aBl }}{{- $blJson = index $annos $aBl }}{{- end }}
{{- $purge := $defaultPurge }}{{- if hasKey $annos $aPurge }}{{- $purge = index $annos $aPurge }}{{- end }}
{{- /* #12, same rules: absent inherits, present is passed verbatim and
       checked in the child, so a bad value fails one tenant. */ -}}
{{- $swJson := $.parameters.defaultSyncWindowsJson }}{{- if hasKey $annos $aSW }}{{- $swJson = index $annos $aSW }}{{- end }}
{{- $or := $.parameters.defaultOrphanedResources }}{{- if hasKey $annos $aOR }}{{- $or = index $annos $aOR }}{{- end }}
{{- /* sourceRepos has no shareable default: one org-wide value would let every
       tenant deploy from every other tenant's repos. The only safe shared form
       is a pattern templated on the tenant name. A pattern with no placeholder
       is simply a literal, which is why there is no separate literal option. */ -}}
{{- $reposJson := "" }}
{{- if hasKey $annos $aRepos }}
{{- $reposJson = index $annos $aRepos }}
{{- else if $reposPatterns }}
{{- $repos := list }}
{{- range $p := $reposPatterns }}{{- $repos = append $repos (replace "{{tenant}}" $name (replace "{{ tenant }}" $name $p)) }}{{- end }}
{{- $reposJson = toJson $repos }}
{{- else }}
{{- $reposJson = "[]" }}
{{- end }}
{{- /* The `tenant-` segment is not decoration. Children used to be named
       mto-argocd-<tenant>, so a Tenant called `parent` produced
       mto-argocd-parent -- the chart's OWN parent TemplateInstance. The parent
       would then render a TemplateInstance over itself: template-operator-v2
       applies it, the parent's objectFieldRefs are replaced by the child's
       parameters, it stops ranging over tenants, and every other child is
       orphaned and reaped along with its AppProject. Tenant-triggerable, from
       a name anyone might choose. Child names now live under a prefix the
       parent's name cannot occupy. */}}
---
apiVersion: templates.v2.stakater.com/v2alpha1
kind: TemplateInstance
metadata:
  name: mto-argocd-tenant-{{ $name }}
  namespace: {{ $opNs }}
spec:
  templateRef:
    name: {{ $childTemplate }}
    namespace: {{ $opNs }}
  parameters:
    - name: tenant
      value: {{ $name | quote }}
    - name: argoNamespace
      value: {{ $argoNs | quote }}
    - name: namespacesJson
      value: {{ toJson $mine | quote }}
    - name: whitelistJson
      value: {{ $wlJson | quote }}
    - name: blacklistJson
      value: {{ $blJson | quote }}
    - name: sourceReposJson
      value: {{ $reposJson | quote }}
    - name: privilegedGroupsJson
      value: {{ toJson $privGroups | quote }}
    - name: tenantGroupsJson
      value: {{ toJson $tg | quote }}
    - name: tenantUsersJson
      value: {{ toJson $tu | quote }}
    - name: onDeletePurge
      value: {{ $purge | quote }}
    - name: controlPlaneDestination
      value: {{ ternary "true" "false" $allowCP | quote }}
    - name: syncWindowsJson
      value: {{ $swJson | quote }}
    - name: orphanedResources
      value: {{ $or | quote }}
{{- /* #9. The tenant's Application namespace, checked here because only the
       parent sees the privileged patterns. A refusal is passed down, not
       acted on: the child fails, so one tenant breaks, not the fleet.

       Listed only while it is one of this tenant's own namespaces -- it
       exists and carries this tenant's label -- exactly like a destination.
       Whoever can write Applications in a source namespace can use this
       AppProject, so a namespace that does not exist yet, or that belongs to
       someone else, must not be listed. When MTO creates it, the Namespace
       event re-renders the parent and it appears. */ -}}
{{- $sns := list }}
{{- $snRefused := list }}
{{- if $snOn }}
{{- $sn := replace "{{tenant}}" $name (replace "{{ tenant }}" $name $snPattern) }}
{{- if eq $sn $argoNs }}{{- $snRefused = append $snRefused (printf "source namespace %q is the ArgoCD namespace: an Application there may reference any AppProject" $sn) }}{{- end }}
{{- range $p := $privNs }}{{- if mustRegexMatch $p $sn }}{{- $snRefused = append $snRefused (printf "source namespace %q matches privileged pattern %q" $sn $p) }}{{- end }}{{- end }}
{{- if has $sn $mine }}{{- $sns = list $sn }}{{- end }}
{{- end }}
    - name: sourceNamespacesJson
      value: {{ toJson $sns | quote }}
    - name: sourceNamespacesRefusedJson
      value: {{ toJson $snRefused | quote }}
{{- end }}
{{- end }}
