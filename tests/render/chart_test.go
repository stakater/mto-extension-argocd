package render

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// A blank argocd.namespace does not render zero children -- it renders one per
// tenant, each emitting an AppProject with an empty namespace that the API
// server rejects, so every tenant breaks at once. The values.schema.json
// minLength catches it at install time; TestTemplateGuardsHoldWithoutSchema
// covers the render paths that skip the schema.
func TestSchemaRefusesBlankArgoNamespace(t *testing.T) {
	_, err := HelmTemplate(t, "", nil)
	if err == nil {
		t.Fatal("blank argocd.namespace rendered; it must be refused")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("refusal should name the namespace field, got: %v", err)
	}
}

// Every fail-safe _helpers.tpl re-implements exists because values.schema.json
// is skippable (helm >= 3.16 --skip-schema-validation, and Helm SDK render
// paths that never apply it). So each guard is exercised here WITHOUT the
// schema in front of it: in every other test the schema masks the guard, and
// deleting an include from the parent template would leave the whole suite
// green while the skip-schema path ships the misconfiguration.
func TestTemplateGuardsHoldWithoutSchema(t *testing.T) {
	cases := []struct {
		name    string
		sets    map[string]string
		json    map[string]string // applied via --set-json, for null and structured shapes
		wantErr string
	}{
		// A typo'd mode emits zero children and orphan reaping then deletes
		// every AppProject the release manages -- the fleet-delete path.
		{name: "invalid scaffolding.mode",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "scaffolding.mode": "Wrong"},
			wantErr: "must be one of Always, OnLabel, OnAnnotation, Never"},
		{name: "invalid sync.mode",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "sync.mode": "bogus"},
			wantErr: "must be one of off, revert, strict"},
		// sync stripped entirely: the helper must refuse a missing mode, not
		// quietly fall back -- an earlier draft defaulted to "revert" here,
		// silently contradicting the shipped strict default on exactly this
		// path. Reintroducing any fallback turns this row red.
		{name: "missing sync.mode",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"sync": "null"},
			wantErr: "must be one of off, revert, strict"},
		{name: "blank argocd.namespace",
			sets:    map[string]string{},
			wantErr: "argocd.namespace is required"},
		// The whole section null, not just the field blank. NOTES.txt used to
		// nil-pointer before this guard could speak.
		{name: "null argocd",
			json:    map[string]string{"argocd": "null"},
			wantErr: "argocd.namespace is required"},
		{name: "null scaffolding",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"scaffolding": "null"},
			wantErr: "must be a non-empty annotation key"},
		{name: "unrecognised sourceRepos placeholder",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceRepos.pattern[0]": "https://github.com/acme/${tenant}-gitops"},
			wantErr: "unrecognised placeholder"},
		// The chart's earlier shape was one string. Without this guard a string
		// would range as nothing and silently drop every tenant's sourceRepos.
		{name: "sourceRepos pattern as a plain string",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceRepos.pattern": "https://github.com/acme/{{ tenant }}-gitops"},
			wantErr: "argocd.sourceRepos.pattern must be a list of strings"},
		{name: "empty sourceRepos pattern entry",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceRepos.pattern": `["https://github.com/acme/{{ tenant }}-gitops", ""]`},
			wantErr: "argocd.sourceRepos.pattern contains an empty entry"},
		{name: "non-string sourceRepos pattern entry",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceRepos.pattern": `["https://github.com/acme/{{ tenant }}-gitops", 123]`},
			wantErr: "argocd.sourceRepos.pattern entries must be strings"},
		// ArgoCD's API refuses a project that lists a repo twice, and the two
		// placeholder spellings expand to the same repo.
		{name: "duplicate sourceRepos pattern entry",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceRepos.pattern": `["https://github.com/acme/{{ tenant }}-gitops", "https://github.com/acme/{{tenant}}-gitops"]`},
			wantErr: "lists \"https://github.com/acme/{{ tenant }}-gitops\" twice"},
		{name: "empty scaffolding annotation key",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "scaffolding.annotations.enabled": ""},
			wantErr: "scaffolding.annotations.enabled must be a non-empty annotation key"},
		{name: "empty scaffolding label key",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "scaffolding.labels.enabled": ""},
			wantErr: "scaffolding.labels.enabled must be a non-empty label key"},
		{name: "empty tenantNamespaceLabel",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "tenantNamespaceLabel": ""},
			wantErr: "tenantNamespaceLabel must be a non-empty label key"},
		// null renders "<nil>" into every child's on-delete-purge parameter:
		// fleet-wide failure blamed on tenant annotations. The schema's
		// required marker catches it there; this is the skip-schema twin.
		{name: "null onDeletePurgeAppProject",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"onDeletePurgeAppProject": "null"},
			wantErr: "onDeletePurgeAppProject must be true or false"},
		// A scalar here renders defaultWhitelistJson as a JSON string and every
		// child then dies on-cluster with a range type error.
		{name: "non-list clusterResourceWhitelist",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","clusterResourceWhitelist":"oops"}`},
			wantErr: "argocd.clusterResourceWhitelist must be a list"},
		// A scalar here fails the parent's `has` -- every tenant at once.
		{name: "non-list reservedProjectNames",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","reservedProjectNames":"platform"}`},
			wantErr: "argocd.reservedProjectNames must be a list"},
		{name: "empty reservedProjectNames entry",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.reservedProjectNames": `["platform", ""]`},
			wantErr: "argocd.reservedProjectNames entries must be non-empty strings"},
		// A malformed namespace name passes minLength but the API server rejects
		// every rendered AppProject on cluster, late. Space + uppercase here.
		{name: "malformed argocd.namespace",
			sets:    map[string]string{"argocd.namespace": "Argo CD"},
			wantErr: "not a valid namespace name"},
		// An invalid label key is the quietest misconfiguration: hasKey/dig never
		// match it, so the override or opt-in it names is silently ignored.
		{name: "malformed tenantNamespaceLabel",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "tenantNamespaceLabel": "not a label key!!"},
			wantErr: "not a valid Kubernetes label"},
		// The override keys and the opt-in key share the Tenant annotation map;
		// a duplicate makes the opt-in marker and an override read each other.
		// #9. A string "false" would turn source namespaces on.
		{name: "non-boolean argocd.sourceNamespaces.enabled",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceNamespaces.enabled": `"false"`},
			wantErr: "argocd.sourceNamespaces.enabled must be true or false"},
		{name: "empty argocd.sourceNamespaces.pattern",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceNamespaces.pattern": ""},
			wantErr: "argocd.sourceNamespaces.pattern must be a non-empty string"},
		// Without {{ tenant }} every AppProject lists the same namespace; a
		// glob matches other tenants' namespaces. Both are cross-tenant.
		{name: "sourceNamespaces pattern without the tenant",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceNamespaces.pattern": "shared-apps"},
			wantErr: "argocd.sourceNamespaces.pattern must contain {{ tenant }}"},
		{name: "sourceNamespaces glob pattern",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceNamespaces.pattern": `"{{ tenant }}*"`},
			wantErr: "must expand to a plain namespace name"},
		{name: "unrecognised sourceNamespaces placeholder",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.sourceNamespaces.pattern": `"{{ tenant }}-${env}"`},
			wantErr: "unrecognised placeholder"},
		// #15. A string "false" would turn impersonation on.
		{name: "non-boolean argocd.impersonation.enabled",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.impersonation.enabled": `"false"`},
			wantErr: "argocd.impersonation.enabled must be true or false"},
		// A glob here would be a catch-all in disguise; an invalid name never
		// matches and every sync fails.
		{name: "invalid argocd.impersonation.serviceAccount",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.impersonation.serviceAccount": "*"},
			wantErr: "argocd.impersonation.serviceAccount must be a ServiceAccount name"},
		// #10. Without {{ tenant }} every tenant gets the same group:
		// cross-tenant access. Without {{ tier }} viewers get the owner group.
		{name: "invalid identity.mode",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "identity.mode": "groups"},
			wantErr: "identity.mode must be one of accessControl, groupPattern, both"},
		{name: "identity.groupPattern without the tenant",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "identity.groupPattern": "argocd-{{ tier }}"},
			wantErr: "identity.groupPattern must contain {{ tenant }}"},
		{name: "identity.groupPattern without the tier",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "identity.groupPattern": "tenant-{{ tenant }}"},
			wantErr: "identity.groupPattern must contain {{ tier }}"},
		// #12.
		{name: "invalid argocd.orphanedResources",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "argocd.orphanedResources": "yes"},
			wantErr: "argocd.orphanedResources must be one of off, warn, monitor"},
		{name: "non-list argocd.syncWindows",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","syncWindows":"nightly"}`},
			wantErr: "argocd.syncWindows must be a list"},
		// The default reaches every tenant: a bad schedule or duration there
		// would fail every child, so it is refused at install time.
		{name: "argocd.syncWindows default with a bad cron",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","syncWindows":[{"kind":"deny","schedule":"every night","duration":"8h"}]}`},
			wantErr: "must be a cron expression with five fields"},
		{name: "argocd.syncWindows default with a bad duration",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","syncWindows":[{"kind":"deny","schedule":"0 22 * * *","duration":"8 hours"}]}`},
			wantErr: "must be a Go duration"},
		// #13. ArgoCD before 3.3 drops a whitelist name and allows every object
		// of the kind, so a name needs the explicit 3.3+ confirmation.
		{name: "non-boolean argocd.allowClusterResourceNames",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.allowClusterResourceNames": `"true"`},
			wantErr: "argocd.allowClusterResourceNames must be true or false"},
		{name: "a whitelist name without allowClusterResourceNames",
			json:    map[string]string{"argocd": `{"namespace":"argo-cd","clusterResourceWhitelist":[{"group":"","kind":"Namespace","name":"x"}]}`},
			wantErr: "needs ArgoCD 3.3+"},
		// An unrecognised profile must not fall back to either shape silently.
		{name: "invalid roles.profile",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "roles.profile": "everything"},
			wantErr: "roles.profile must be one of leastPrivilege, extensionsCompatible"},
		// A string "false" is truthy in a template: it would grant exec.
		{name: "non-boolean roles.exec.enabled",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"roles.exec.enabled": `"false"`},
			wantErr: "roles.exec.enabled must be true or false"},
		{name: "non-boolean roles.clusters.enabled",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"roles.clusters.enabled": `"false"`},
			wantErr: "roles.clusters.enabled must be true or false"},
		{name: "non-boolean argocd.allowControlPlaneDestination",
			sets:    map[string]string{"argocd.namespace": "argo-cd"},
			json:    map[string]string{"argocd.allowControlPlaneDestination": `"false"`},
			wantErr: "argocd.allowControlPlaneDestination must be true or false"},
		{name: "colliding annotation keys",
			sets:    map[string]string{"argocd.namespace": "argo-cd", "annotations.onDeletePurge": "argocd.tenantoperator.stakater.com/enabled"},
			wantErr: "annotation keys collide"},
	}
	// An empty override key never matches any annotation, so the override --
	// including on-delete-purge -- is silently ignored and the chart default
	// silently wins. Nothing would ever error. All four keys, so dropping one
	// from the validateOverrideKeys range list is caught.
	for _, key := range []string{"sourceRepos", "clusterResourceWhitelist", "namespaceResourceBlacklist", "onDeletePurge", "syncWindows", "orphanedResources"} {
		cases = append(cases, struct {
			name    string
			sets    map[string]string
			json    map[string]string
			wantErr string
		}{
			name:    "empty override annotation key " + key,
			sets:    map[string]string{"argocd.namespace": "argo-cd", "annotations." + key: ""},
			wantErr: "annotations." + key + " must be a non-empty annotation key",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := []string{"--skip-schema-validation"}
			for k, v := range tc.json {
				extra = append(extra, "--set-json", k+"="+v)
			}
			_, err := HelmTemplate(t, "", tc.sets, extra...)
			if err == nil {
				t.Fatal("rendered without the schema; the _helpers.tpl guard must refuse this on its own")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error should contain %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// The reap verbs must be granted regardless of scaffolding.mode, and only
// `create` may be gated. Gating the whole block leaves the TemplateInstance
// finalizer unable to delete what it owns, and teardown blocks forever. The
// same split exists twice: the ClusterRole for AppProjects, and the namespaced
// Role for the child TemplateInstances -- gating either whole block reproduces
// the identical deadlock one resource over.
func TestRBACReapVerbsSurviveModeNever(t *testing.T) {
	for _, mode := range []string{"Always", "Never"} {
		t.Run(mode, func(t *testing.T) {
			docs := MustHelmTemplate(t, "", map[string]string{
				"argocd.namespace": "openshift-gitops",
				"scaffolding.mode": mode,
			})
			for _, target := range []struct {
				kind, name, apiGroup, resource string
			}{
				{"ClusterRole", "mto-extension-argocd-write", "argoproj.io", "appprojects"},
				{"Role", "mto-extension-argocd-children", "templates.v2.stakater.com", "templateinstances"},
			} {
				verbs := roleVerbs(t, docs, target.kind, target.name, target.apiGroup, target.resource)

				for _, v := range []string{"get", "update", "patch", "delete"} {
					if !verbs[v] {
						t.Errorf("mode=%s: %s %s does not grant reap verb %q", mode, target.kind, target.name, v)
					}
				}
				if want := mode != "Never"; verbs["create"] != want {
					t.Errorf("mode=%s: %s %s create granted=%v, want %v", mode, target.kind, target.name, verbs["create"], want)
				}
			}
		})
	}
}

// Never is the documented kill switch: zero children, and orphan reaping then
// removes what the release managed. If a restructured filter ever defaulted to
// "take", Never would silently scaffold everything -- the inverse of a kill
// switch. Only the on-demand e2e lane covered this before; this pins it on
// every PR.
func TestModeNeverEmitsZeroChildren(t *testing.T) {
	rendered, childErrs := TwoStage(t, Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Never"},
		},
		Cluster: map[string]any{
			"tenants": []any{
				// Opted in under both keys, so Never must win over any opt-in.
				map[string]any{"metadata": map[string]any{
					"name":        "opted-in",
					"annotations": map[string]any{"argocd.tenantoperator.stakater.com/enabled": "true"},
					"labels":      map[string]any{"argocd.tenantoperator.stakater.com/enabled": "true"},
				}, "spec": map[string]any{}},
				map[string]any{"metadata": map[string]any{"name": "plain"}, "spec": map[string]any{}},
			},
			"namespaces": []any{},
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{}, "groups": []any{}}},
			},
		},
	})
	if len(childErrs) != 0 {
		t.Fatalf("children failed under mode=Never: %v", childErrs)
	}
	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("decoding render: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("mode=Never emitted %d AppProjects, want 0: the kill switch scaffolds", len(docs))
	}
}

// `off` must emit no sync block at all -- not `mode: off` -- so the CRD
// default applies. The admission suite only proves an off-render is ACCEPTED,
// which a wrong `mode: revert` block would be too.
func TestSyncModeOffEmitsNoSyncBlock(t *testing.T) {
	docs := MustHelmTemplate(t, "sync:\n  mode: \"off\"\n", map[string]string{
		"argocd.namespace": "openshift-gitops",
	})
	for _, name := range []string{parentTemplateName, childTemplateName} {
		tmpl := Find(t, docs, "Template", name)
		if sync, ok := spec(tmpl)["sync"]; ok {
			t.Errorf("%s carries a sync block under mode=off (%v); nothing should be emitted so the CRD default applies", name, sync)
		}
	}
}

// rbac.create=false is for clusters that bind the grants themselves. It must
// remove every RBAC object and nothing else -- the three template-operator-v2
// objects are the chart's whole mechanism and must survive.
func TestRBACCreateFalseOmitsAllRBAC(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{
		"argocd.namespace": "openshift-gitops",
		"rbac.create":      "false",
	})
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		switch kind {
		case "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding":
			t.Errorf("rbac.create=false still renders %s/%v", kind, meta(d)["name"])
		}
	}
	Find(t, docs, "Template", parentTemplateName)
	Find(t, docs, "Template", childTemplateName)
	Find(t, docs, "TemplateInstance", parentTemplateName)
}

// Every RBAC object must OUTLIVE the release on teardown, or the operator's
// finalizers deadlock the whole release in Terminating. Helm and
// ArgoCD honour different annotations, so both must be present -- dropping
// either silently brings the deadlock back on one of the two teardown paths.
func TestRBACIsRetainedOnTeardown(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})

	var rbac int
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		switch kind {
		case "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding":
		default:
			continue
		}
		rbac++
		name, _ := meta(d)["name"].(string)
		annos, _ := meta(d)["annotations"].(map[string]any)
		if got, _ := annos["helm.sh/resource-policy"].(string); got != "keep" {
			t.Errorf("%s/%s: helm.sh/resource-policy = %q, want keep (helm uninstall would delete it and deadlock the finalizers)", kind, name, got)
		}
		// Prune=false survives a sync drop, Delete=false a cascade app delete;
		// resource-policy is Helm-only, so this is not redundant.
		got, _ := annos["argocd.argoproj.io/sync-options"].(string)
		if !strings.Contains(got, "Prune=false") || !strings.Contains(got, "Delete=false") {
			t.Errorf("%s/%s: argocd.argoproj.io/sync-options = %q, want both Prune=false and Delete=false", kind, name, got)
		}
	}
	// Guard against the block vanishing and the test passing vacuously.
	if rbac == 0 {
		t.Fatal("no RBAC objects rendered; the retention assertions never ran")
	}
}

// The parent's objectFieldRefs pin apiVersions (Tenant v1beta3,
// IntegrationConfig v1beta1). Whether those versions are actually SERVED was
// otherwise checked only by the on-demand e2e lane -- and the
// v1beta1-vs-v1beta3 storage-version trap was real (see tests/e2e). The CRDs
// vendored for the admission suite are the same ones the kind lane installs,
// so pin the reference against them on every PR.
func TestParentObjectFieldRefVersionsAreServed(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})
	ti := Find(t, docs, "TemplateInstance", parentTemplateName)

	crdFiles, err := filepath.Glob(filepath.Join("..", "crds", "*.yaml"))
	if err != nil || len(crdFiles) == 0 {
		t.Fatalf("no CRDs found in tests/crds: %v", err)
	}
	served := map[string]bool{} // "group/version/Kind" -> served
	for _, f := range crdFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		crds, err := Documents(string(raw))
		if err != nil {
			t.Fatalf("decoding %s: %v", f, err)
		}
		for _, crd := range crds {
			sp := spec(crd)
			group, _ := sp["group"].(string)
			names, _ := sp["names"].(map[string]any)
			kind, _ := names["kind"].(string)
			versions, _ := sp["versions"].([]any)
			for _, v := range versions {
				vm, _ := v.(map[string]any)
				if s, _ := vm["served"].(bool); !s {
					continue
				}
				version, _ := vm["name"].(string)
				served[group+"/"+version+"/"+kind] = true
			}
		}
	}

	params, _ := spec(ti)["parameters"].([]any)
	checked := 0
	for _, p := range params {
		pm, _ := p.(map[string]any)
		vf, _ := pm["valueFrom"].(map[string]any)
		ref, _ := vf["objectFieldRef"].(map[string]any)
		apiVersion, _ := ref["apiVersion"].(string)
		kind, _ := ref["kind"].(string)
		if apiVersion == "" || !strings.Contains(apiVersion, "/") {
			continue // core-group Namespace has no CRD to check against
		}
		checked++
		if !served[apiVersion+"/"+kind] {
			t.Errorf("parameter %v references %s %s, which no CRD in tests/crds serves", pm["name"], apiVersion, kind)
		}
	}
	if checked != 2 {
		t.Errorf("expected to check 2 CRD-backed objectFieldRefs (Tenant, IntegrationConfig), checked %d", checked)
	}
}

// Masking a failed IntegrationConfig read yields an empty privileged list, and
// privileged namespaces then land in every tenant's destinations. Failing the
// parent is fail-closed, and matches MTO's own [".*"] fallback.
func TestIntegrationConfigParameterHasNoDefaultValue(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})
	ti := Find(t, docs, "TemplateInstance", parentTemplateName)

	params, _ := spec(ti)["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] != "integrationConfig" {
			continue
		}
		vf, _ := pm["valueFrom"].(map[string]any)
		ref, _ := vf["objectFieldRef"].(map[string]any)
		if dv, ok := ref["defaultValue"]; ok {
			t.Fatalf("integrationConfig has defaultValue=%v; masking a failed read puts privileged namespaces into tenants' destinations", dv)
		}
		return
	}
	t.Fatal("no integrationConfig parameter on the parent TemplateInstance")
}

// Under strict, the admission webhook refuses any UPDATE touching an enforced
// field -- including the one template-operator-v2 uses to manage its own
// finalizer. Without the exemption it locks itself out and teardown deadlocks.
func TestStrictSyncRequiresFinalizersExemption(t *testing.T) {
	_, err := HelmTemplate(t, "sync:\n  mode: strict\n  ignoreFields: []\n", map[string]string{
		"argocd.namespace": "openshift-gitops",
	})
	if err == nil {
		t.Fatal("sync.mode=strict rendered with no metadata.finalizers exemption")
	}
	if !strings.Contains(err.Error(), "metadata.finalizers") {
		t.Errorf("error should name metadata.finalizers, got: %v", err)
	}

	// An unrelated exemption is not enough -- it has to be the right one.
	_, err = HelmTemplate(t, "sync:\n  mode: strict\n  ignoreFields: [\"spec.description\"]\n", map[string]string{
		"argocd.namespace": "openshift-gitops",
	})
	if err == nil {
		t.Error("sync.mode=strict accepted an exemption list without metadata.finalizers")
	}

	// ArgoCD writes project role tokens into spec.roles[].jwtTokens -- the
	// spec, not status -- so `argocd proj role create-token` is denied without
	// this exemption. status is stripped automatically; the spec field is not.
	_, err = HelmTemplate(t, "sync:\n  mode: strict\n  ignoreFields: [\"metadata.finalizers\"]\n", map[string]string{
		"argocd.namespace": "openshift-gitops",
	})
	if err == nil {
		t.Fatal("sync.mode=strict accepted a list without spec.roles[*].jwtTokens")
	}
	if !strings.Contains(err.Error(), "jwtTokens") {
		t.Errorf("error should name jwtTokens, got: %v", err)
	}
}

// The shipped default is strict WITH the exemption, because under revert a
// deleted AppProject wedges in Terminating forever. If someone relaxes this
// default, it should be a deliberate, visible change.
func TestDefaultSyncModeIsStrictAndSafe(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})
	for _, name := range []string{parentTemplateName, childTemplateName} {
		tmpl := Find(t, docs, "Template", name)
		sync, _ := spec(tmpl)["sync"].(map[string]any)
		if sync == nil {
			t.Fatalf("%s has no sync block", name)
		}
		if sync["mode"] != "strict" {
			t.Errorf("%s sync.mode = %v, want strict", name, sync["mode"])
		}
		ignore, _ := sync["ignoreFields"].([]any)
		for _, want := range []string{"metadata.finalizers", "spec.roles[*].jwtTokens"} {
			found := false
			for _, f := range ignore {
				if f == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s ignoreFields = %v, missing %s", name, ignore, want)
			}
		}
	}
}

// Only `{{ tenant }}` and `{{tenant}}` are substituted into each
// argocd.sourceRepos.pattern entry. Any other placeholder form ships its braces
// verbatim into a live AppProject's sourceRepos, granting access to a
// repository that cannot exist -- silently, and it reads correctly at a
// glance. The patterns are chart-wide values, so refusing the install is both
// the earliest and the safest place to catch it.
func TestSourceReposPatternRejectsUnknownPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		valid   bool
	}{
		{"https://github.com/acme/{{ tenant }}-gitops", true},
		{"https://github.com/acme/{{tenant}}-gitops", true},
		{"https://github.com/acme/shared-gitops", true}, // no placeholder is a literal
		{"https://github.com/acme/{{ .tenant }}-gitops", false},
		{"https://github.com/acme/{{ .Tenant }}-gitops", false},
		{"https://github.com/acme/{{ tenantName }}-gitops", false},
		// Brace-free placeholder syntaxes ship just as silently: an earlier
		// validator only looked for leftover "{{", so ${tenant} -- which the
		// helper's own comment listed as caught -- sailed straight through.
		{"https://github.com/acme/${tenant}-gitops", false},
		{"https://github.com/acme/$(tenant)-gitops", false},
		{"https://github.com/acme/%s-gitops", false},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			_, err := HelmTemplate(t, "", map[string]string{
				"argocd.namespace":              "openshift-gitops",
				"argocd.sourceRepos.pattern[0]": tc.pattern,
			})
			if tc.valid && err != nil {
				t.Errorf("pattern should be accepted: %v", err)
			}
			if !tc.valid {
				if err == nil {
					t.Error("unrecognised placeholder was accepted; its braces would ship into a live AppProject")
				} else if !strings.Contains(err.Error(), "unrecognised placeholder") {
					t.Errorf("error should name the problem, got: %v", err)
				}
			}
		})
	}
}

// The parent aggregates each child's status so one object answers for the whole
// fleet: ready to alert on, reason to group by, errors to read.
func TestParentAggregatesReadyStatusReasonAndErrors(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})
	parent := Find(t, docs, "Template", parentTemplateName)

	outputs, _ := spec(parent)["outputs"].([]any)
	if len(outputs) == 0 {
		t.Fatal("parent Template declares no outputs")
	}
	out, _ := outputs[0].(map[string]any)

	if _, ok := out["fromAll"].(map[string]any); !ok {
		t.Error("parent output should aggregate with fromAll, not a single from")
	}
	if out["keyTemplate"] != "{.metadata.name}" {
		t.Errorf("keyTemplate = %v, want the child's name so the map is keyed per tenant", out["keyTemplate"])
	}

	// errors selects conditions by STATUS, not by type: the Ready condition
	// only summarises ("Template rendering failed") while the cause sits on
	// whichever stage actually broke. Selecting status==False gets the real
	// message and stays empty on a healthy tenant.
	want := map[string]string{
		"ready":  "{.status.conditions[?(@.type=='Ready')].status}",
		"reason": "{.status.conditions[?(@.type=='Ready')].reason}",
		"errors": "{.status.conditions[?(@.status=='False')].message}",
	}
	got := map[string]string{}
	fields, _ := out["fields"].([]any)
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		name, _ := fm["name"].(string)
		path, _ := fm["jsonPath"].(string)
		got[name] = path
	}
	for name, path := range want {
		if got[name] != path {
			t.Errorf("output field %q jsonPath = %q, want %q", name, got[name], path)
		}
	}
}

// Every RBAC name is fixed, like the Template names. The chart supports one
// release per cluster, and a release prefix on only some names would suggest
// an isolation it does not give. NOTES.txt and the docs name these objects, so
// a rename must be deliberate.
//
// Rendered under two releases in two namespaces. The default release name is
// "argocd", so a name built from .Release.Name could render as the fixed name
// under it and pass by accident.
func TestRBACNamesAreFixed(t *testing.T) {
	want := []string{
		"ClusterRole/mto-extension-argocd-observe",
		"ClusterRole/mto-extension-argocd-write",
		"ClusterRoleBinding/mto-extension-argocd-observe",
		"Role/mto-extension-argocd-children",
		"Role/mto-extension-argocd-integrationconfig-read",
		"RoleBinding/mto-extension-argocd-children",
		"RoleBinding/mto-extension-argocd-integrationconfig-read",
		"RoleBinding/mto-extension-argocd-write",
	}
	for _, r := range []struct{ release, namespace string }{
		{"argocd", "default"},
		{"other-release", "other-namespace"},
	} {
		out, err := HelmTemplateRelease(t, r.release, "", map[string]string{"argocd.namespace": "openshift-gitops"}, "--namespace", r.namespace)
		if err != nil {
			t.Fatalf("rendering chart as %s: %v", r.release, err)
		}
		docs, err := Documents(out)
		if err != nil {
			t.Fatalf("decoding chart output: %v", err)
		}
		var got []string
		for _, d := range docs {
			kind, _ := d["kind"].(string)
			if !strings.HasSuffix(kind, "Role") && !strings.HasSuffix(kind, "RoleBinding") {
				continue
			}
			name, _ := meta(d)["name"].(string)
			got = append(got, kind+"/"+name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("RBAC objects for release %s in %s\n got  %v\n want %v", r.release, r.namespace, got, want)
		}
	}
}

// #27. By default every binding goes to the account MTO runs
// template-operator-v2 as (components.templateOperatorV2.mode: Managed). The
// old default named a standalone install's account, so on an MTO-managed
// operator the install succeeded and nothing was ever reconciled.
func TestRBACDefaultsToTheMTOManagedServiceAccount(t *testing.T) {
	docs := MustHelmTemplate(t, "", map[string]string{"argocd.namespace": "openshift-gitops"})
	n := 0
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		if !strings.HasSuffix(kind, "Binding") {
			continue
		}
		n++
		subjects, _ := d["subjects"].([]any)
		if len(subjects) != 1 {
			t.Fatalf("%s/%v has %d subjects, want 1", kind, meta(d)["name"], len(subjects))
		}
		s, _ := subjects[0].(map[string]any)
		if s["kind"] != "ServiceAccount" || s["namespace"] != "multi-tenant-operator" || s["name"] != "tenant-operator-template-operator-v2" {
			t.Errorf("%s/%v binds %v, want ServiceAccount multi-tenant-operator/tenant-operator-template-operator-v2", kind, meta(d)["name"], s)
		}
	}
	if n != 4 {
		t.Errorf("checked %d bindings, want 4", n)
	}
}

// roleVerbs collects the verbs a Role or ClusterRole grants on ONE
// apiGroup/resource pair. Scoping matters: merging verbs across every rule
// would let a `create` granted on some unrelated resource satisfy a check
// about appprojects.
func roleVerbs(t *testing.T, docs []map[string]any, kind, name, apiGroup, resource string) map[string]bool {
	t.Helper()
	cr := Find(t, docs, kind, name)
	out := map[string]bool{}
	rules, _ := cr["rules"].([]any)
	for _, r := range rules {
		rm, _ := r.(map[string]any)
		if !containsString(rm["apiGroups"], apiGroup) || !containsString(rm["resources"], resource) {
			continue
		}
		vs, _ := rm["verbs"].([]any)
		for _, v := range vs {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s %s grants nothing on %s/%s", kind, name, apiGroup, resource)
	}
	return out
}

func containsString(list any, want string) bool {
	items, _ := list.([]any)
	for _, i := range items {
		if s, ok := i.(string); ok && s == want {
			return true
		}
	}
	return false
}
