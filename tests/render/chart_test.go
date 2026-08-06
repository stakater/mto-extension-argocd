package render

import (
	"strings"
	"testing"
)

// A blank argocd.namespace does not render zero children -- it renders one per
// tenant, each emitting an AppProject with an empty namespace that the API
// server rejects, so every tenant breaks at once. The values.schema.json
// minLength is the only thing between a typo and that outcome.
func TestSchemaRefusesBlankArgoNamespace(t *testing.T) {
	if _, err := HelmTemplate(t, "", nil); err == nil {
		t.Fatal("blank argocd.namespace rendered; it must be refused")
	}
}

// The reap verbs must be granted regardless of scaffolding.mode, and only
// `create` may be gated. Gating the whole block leaves the TemplateInstance
// finalizer unable to delete what it owns, and teardown blocks forever.
func TestRBACReapVerbsSurviveModeNever(t *testing.T) {
	for _, mode := range []string{"Always", "Never"} {
		t.Run(mode, func(t *testing.T) {
			docs := MustHelmTemplate(t, "", map[string]string{
				"argocd.namespace": "openshift-gitops",
				"scaffolding.mode": mode,
			})
			verbs := clusterRoleVerbs(t, docs, writeClusterRoleName(t, docs), "argoproj.io", "appprojects")

			for _, v := range []string{"get", "update", "patch", "delete"} {
				if !verbs[v] {
					t.Errorf("mode=%s does not grant reap verb %q", mode, v)
				}
			}
			if want := mode != "Never"; verbs["create"] != want {
				t.Errorf("mode=%s: create granted=%v, want %v", mode, verbs["create"], want)
			}
		})
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
// deleted AppProject wedges in Terminating forever (docs/upstream-issues.md,
// TOV2-2). If someone relaxes this default, it should be a deliberate,
// visible change.
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

// Only `{{ tenant }}` and `{{tenant}}` are substituted into
// argocd.sourceRepos.pattern. Any other placeholder form ships its braces
// verbatim into a live AppProject's sourceRepos, granting access to a
// repository that cannot exist -- silently, and it reads correctly at a
// glance. The pattern is one chart-wide value, so refusing the install is both
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
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			_, err := HelmTemplate(t, "", map[string]string{
				"argocd.namespace":           "openshift-gitops",
				"argocd.sourceRepos.pattern": tc.pattern,
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

// writeClusterRoleName finds the AppProject-write ClusterRole. Its name is
// release-scoped -- fixed cluster-scoped names make a second install anywhere
// on the cluster fail with "invalid ownership metadata" -- so the test
// discovers it rather than hardcoding it.
func writeClusterRoleName(t *testing.T, docs []map[string]any) string {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "ClusterRole" {
			continue
		}
		name, _ := meta(d)["name"].(string)
		if strings.HasSuffix(name, "-argocd-write") {
			return name
		}
	}
	t.Fatal("no AppProject-write ClusterRole in the rendered chart")
	return ""
}

// clusterRoleVerbs collects the verbs granted on ONE apiGroup/resource pair.
// Scoping matters: merging verbs across every rule would let a `create` granted
// on some unrelated resource satisfy a check about appprojects.
func clusterRoleVerbs(t *testing.T, docs []map[string]any, name, apiGroup, resource string) map[string]bool {
	t.Helper()
	cr := Find(t, docs, "ClusterRole", name)
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
		t.Fatalf("ClusterRole %s grants nothing on %s/%s", name, apiGroup, resource)
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
