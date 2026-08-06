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
{{- $opNs := .parameters.operatorNamespace }}
{{- $childTemplate := .parameters.childTemplateName }}
{{- $argoNs := .parameters.argoNamespace }}
{{- $nsLabel := .parameters.tenantNamespaceLabel }}
{{- $mode := .parameters.scaffoldingMode }}
{{- $annoKey := .parameters.scaffoldingAnnotationKey }}
{{- $labelKey := .parameters.scaffoldingLabelKey }}
{{- $defaultWl := .parameters.defaultWhitelistJson }}
{{- $defaultBl := .parameters.defaultBlacklistJson }}
{{- $reposPattern := .parameters.sourceReposPattern }}
{{- $defaultPurge := .parameters.defaultOnDeletePurge }}
{{- $aRepos := .parameters.annotationSourceRepos }}
{{- $aWl := .parameters.annotationClusterResourceWhitelist }}
{{- $aBl := .parameters.annotationNamespaceResourceBlacklist }}
{{- $aPurge := .parameters.annotationOnDeletePurge }}
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
*/ -}}
{{- $ic := .parameters.integrationConfig }}
{{- $priv := dig "accessControl" "privileged" dict $ic }}
{{- if not (hasKey $priv "namespaces") }}
{{- fail "IntegrationConfig .spec.accessControl.privileged.namespaces is absent; refusing to render because an empty privileged list would put privileged namespaces into every tenant's destinations. Set it explicitly, to [] if the cluster genuinely has none." }}
{{- end }}
{{- $privNs := $priv.namespaces | default list }}
{{- $privGroups := $priv.groups | default list }}
{{- range $t := .parameters.tenants }}
{{- $name := $t.metadata.name }}
{{- $labels := default dict $t.metadata.labels }}
{{- $annos := default dict $t.metadata.annotations }}
{{- /* Which tenants get an AppProject. No child emitted means no AppProject --
       and, because template-operator-v2 reaps orphans, narrowing this filter
       deletes the AppProjects it excludes. */ -}}
{{- $take := false }}
{{- if eq $mode "Always" }}{{- $take = true }}
{{- else if eq $mode "OnLabel" }}{{- $take = eq (dig $labelKey "" $labels) "true" }}
{{- else if eq $mode "OnAnnotation" }}{{- $take = eq (dig $annoKey "" $annos) "true" }}
{{- end }}
{{- if $take }}
{{- /* This tenant's namespaces, minus the privileged ones.

       regexMatch is UNANCHORED, matching Go's regexp.MatchString in the
       controller this replaces. A pattern of "openshift" therefore also
       excludes "my-openshift-tools". That is existing behaviour, reproduced
       on purpose -- and it is why the privileged fixtures are the first test
       in this repo: MTO's managed-prefix webhook makes this path untestable
       on a real cluster, and getting it wrong puts one tenant's namespaces
       into another tenant's AppProject. */ -}}
{{- $mine := list }}
{{- range $ns := $.parameters.namespaces }}
{{- $nsLabels := default dict $ns.metadata.labels }}
{{- if eq (dig $nsLabel "" $nsLabels) $name }}
{{- $isPriv := false }}
{{- /* mustRegexMatch, not regexMatch: the plain form discards the compile
       error, so a typo'd pattern in the cluster's IntegrationConfig silently
       stops filtering -- for every tenant at once. */ -}}
{{- range $p := $privNs }}{{- if mustRegexMatch $p $ns.metadata.name }}{{- $isPriv = true }}{{- end }}{{- end }}
{{- if not $isPriv }}{{- $mine = append $mine $ns.metadata.name }}{{- end }}
{{- end }}
{{- end }}
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
{{- $tspec := default dict $t.spec }}
{{- $ac := default dict $tspec.accessControl }}
{{- /* Each role block is defaulted too. `dig "owners" "groups" list $ac`
       looks safe but errors when accessControl.owners is explicitly null --
       dig needs a map at every level and gets nil. Defaulting each level in
       turn is the only form that survives every shape. */ -}}
{{- $owners := default dict $ac.owners }}
{{- $editors := default dict $ac.editors }}
{{- $viewers := default dict $ac.viewers }}
{{- $tg := dict
      "owner" (default list $owners.groups)
      "edit"  (default list $editors.groups)
      "view"  (default list $viewers.groups) }}
{{- $tu := dict
      "owner" (default list $owners.users)
      "edit"  (default list $editors.users)
      "view"  (default list $viewers.users) }}
{{- /* Per-tenant overrides. An absent annotation inherits the chart default; an
       explicit "[]" overrides with empty. The annotation's value is passed down
       VERBATIM and parsed in the child, so malformed JSON fails one tenant
       instead of the whole parent. */ -}}
{{- $wlJson := dig $aWl "" $annos | default $defaultWl }}
{{- $blJson := dig $aBl "" $annos | default $defaultBl }}
{{- $purge := dig $aPurge "" $annos | default $defaultPurge }}
{{- /* sourceRepos has no shareable default: one org-wide value would let every
       tenant deploy from every other tenant's repos. The only safe shared form
       is a pattern templated on the tenant name. A pattern with no placeholder
       is simply a literal, which is why there is no separate literal option. */ -}}
{{- $reposJson := dig $aRepos "" $annos }}
{{- if not $reposJson }}
{{- if $reposPattern }}
{{- $reposJson = toJson (list (replace "{{tenant}}" $name (replace "{{ tenant }}" $name $reposPattern))) }}
{{- else }}
{{- $reposJson = "[]" }}
{{- end }}
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
{{- end }}
{{- end }}
