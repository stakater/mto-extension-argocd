package render

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Values reaching the child come from the Tenant and the IntegrationConfig,
// which tenant authors control. Unquoted, they are a YAML injection into live
// RBAC: a group named "ok\n        - injected-admin-group" renders as two list
// entries and grants a group nobody configured, and a payload carrying a
// document separator can emit an entire second AppProject naming another
// tenant -- which this chart's RBAC permits and template-operator-v2 applies.
//
// Benign values break too: unquoted, `true` becomes a boolean and `12345` an
// int, neither of which ArgoCD accepts as a group name.
func TestTenantStringsCannotInjectYAML(t *testing.T) {
	const injected = "ok\n        - injected-admin-group"

	rendered, childErrs := TwoStage(t, Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants": []any{
				map[string]any{
					"metadata": map[string]any{"name": "victim"},
					"spec": map[string]any{"accessControl": map[string]any{
						"owners": map[string]any{
							"groups": []any{injected, "true", "12345"},
							"users":  []any{"a@b.c\n        - injected-user"},
						},
					}},
				},
			},
			"namespaces": []any{},
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{
					"namespaces": []any{},
					"groups":     []any{"priv\n        - injected-priv"},
				}},
			},
		},
	})
	for name, err := range childErrs {
		t.Fatalf("child %s failed: %v", name, err)
	}

	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("render is not valid YAML: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d AppProjects, want 1 — a payload emitted an extra document", len(docs))
	}

	spec, _ := docs[0]["spec"].(map[string]any)
	roles, _ := spec["roles"].([]any)
	owner, _ := roles[0].(map[string]any)
	groups, _ := owner["groups"].([]any)

	// generated + 1 privileged + 3 tenant groups + 1 user = 6. Any injected
	// entry shows up as an extra.
	if len(groups) != 6 {
		t.Errorf("owner role has %d groups, want 6 — extra entries mean injection: %v", len(groups), groups)
	}
	for _, g := range groups {
		s, ok := g.(string)
		if !ok {
			t.Errorf("group %v is %T, not a string — an unquoted value was coerced", g, g)
			continue
		}
		if strings.Contains(s, "\n") {
			// A newline INSIDE one quoted string is fine; it is only injection
			// when it splits into separate entries, which the count above
			// catches.
			continue
		}
		if strings.HasPrefix(s, "injected") {
			t.Errorf("injected entry %q became a group of its own", s)
		}
	}
}

// The privileged-namespace filter must fail closed. `dig` with a dict default
// plus `| default list` silently turns every missing key into an empty
// privileged list, which disables filtering entirely -- the exact inverse of
// the intent, and the failure mode is one tenant's AppProject listing another
// tenant's namespaces.
func TestPrivilegedFilterFailsClosed(t *testing.T) {
	clusterWith := func(ic map[string]any) map[string]any {
		return map[string]any{
			"integrationConfig": ic,
			"tenants":           []any{map[string]any{"metadata": map[string]any{"name": "alpha"}, "spec": map[string]any{}}},
			"namespaces": []any{
				map[string]any{"metadata": map[string]any{"name": "alpha-app", "labels": map[string]any{"stakater.com/tenant": "alpha"}}},
				map[string]any{"metadata": map[string]any{"name": "openshift-monitoring", "labels": map[string]any{"stakater.com/tenant": "alpha"}}},
			},
		}
	}

	for _, tc := range []struct {
		name string
		ic   map[string]any
	}{
		{"no accessControl key", map[string]any{"components": map[string]any{}}},
		{"privileged without namespaces", map[string]any{
			"accessControl": map[string]any{"privileged": map[string]any{"groups": []any{"admins"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { _ = recover() }()
			values, _ := marshalValues(map[string]any{
				"argocd":      map[string]any{"namespace": "argo-cd"},
				"scaffolding": map[string]any{"mode": "Always"},
			})
			docs := MustHelmTemplate(t, values, nil)
			parent := Find(t, docs, "Template", parentTemplateName)
			params := paramMap(parent)
			for k, v := range clusterWith(tc.ic) {
				params[k] = v
			}
			gotmpl, _ := spec(parent)["gotemplate"].(string)
			out, err := Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
			if err == nil {
				t.Fatalf("render succeeded with no privileged.namespaces; destinations would include privileged namespaces:\n%s", out)
			}
			if !strings.Contains(err.Error(), "privileged.namespaces is absent") {
				t.Errorf("failed for the wrong reason: %v", err)
			}
		})
	}

	// An uncompilable pattern must fail too. sprig's regexMatch discards the
	// compile error, so a typo in the cluster's IntegrationConfig would stop
	// filtering for every tenant at once.
	t.Run("uncompilable regex", func(t *testing.T) {
		values, _ := marshalValues(map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		})
		docs := MustHelmTemplate(t, values, nil)
		parent := Find(t, docs, "Template", parentTemplateName)
		params := paramMap(parent)
		for k, v := range clusterWith(map[string]any{
			"accessControl": map[string]any{"privileged": map[string]any{
				"namespaces": []any{"^openshift-["}, "groups": []any{}}},
		}) {
			params[k] = v
		}
		gotmpl, _ := spec(parent)["gotemplate"].(string)
		if out, err := Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"}); err == nil {
			t.Fatalf("an uncompilable privileged pattern rendered successfully:\n%s", out)
		}
	})
}

// The parent must survive every shape a Tenant can take. A failure here kills
// the PARENT, so one malformed Tenant would take every other tenant's
// AppProject down with it. Neither `$t.spec.accessControl` nor
// `dig "spec" "accessControl"` survives all four on its own.
func TestParentSurvivesEveryTenantShape(t *testing.T) {
	shapes := map[string]any{
		"no spec key":         map[string]any{"metadata": map[string]any{"name": "a"}},
		"empty spec":          map[string]any{"metadata": map[string]any{"name": "b"}, "spec": map[string]any{}},
		"null spec":           map[string]any{"metadata": map[string]any{"name": "c"}, "spec": nil},
		"null accessControl":  map[string]any{"metadata": map[string]any{"name": "d"}, "spec": map[string]any{"accessControl": nil}},
		"null owners":         map[string]any{"metadata": map[string]any{"name": "e"}, "spec": map[string]any{"accessControl": map[string]any{"owners": nil}}},
		"owners without keys": map[string]any{"metadata": map[string]any{"name": "f"}, "spec": map[string]any{"accessControl": map[string]any{"owners": map[string]any{}}}},
	}

	var tenants []any
	for _, v := range shapes {
		tenants = append(tenants, v)
	}

	rendered, childErrs := TwoStage(t, Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants":    tenants,
			"namespaces": []any{},
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{}, "groups": []any{}}},
			},
		},
	})
	for name, err := range childErrs {
		t.Errorf("child %s failed: %v", name, err)
	}
	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("render is not valid YAML: %v", err)
	}
	if len(docs) != len(shapes) {
		t.Errorf("got %d AppProjects for %d tenant shapes — a malformed Tenant suppressed others", len(docs), len(shapes))
	}
}

// A Tenant named `parent` used to render a child TemplateInstance called
// mto-argocd-parent -- the chart's OWN parent. template-operator-v2 would apply
// it, replacing the parent's objectFieldRefs with the child's parameters, so it
// stops ranging over tenants and every other child is orphaned and reaped along
// with its AppProject. Tenant-triggerable, from a name anyone might pick.
func TestTenantCannotCollideWithTheParentTemplateInstance(t *testing.T) {
	values, err := marshalValues(map[string]any{
		"argocd":      map[string]any{"namespace": "argo-cd"},
		"scaffolding": map[string]any{"mode": "Always"},
	})
	if err != nil {
		t.Fatalf("values: %v", err)
	}
	docs := MustHelmTemplate(t, values, nil)
	parent := Find(t, docs, "TemplateInstance", parentTemplateName)
	parentName := meta(parent)["name"]

	tmpl := Find(t, docs, "Template", parentTemplateName)
	params := paramMap(tmpl)
	params["tenants"] = []any{
		map[string]any{"metadata": map[string]any{"name": "parent"}, "spec": map[string]any{}},
		map[string]any{"metadata": map[string]any{"name": "child"}, "spec": map[string]any{}},
		map[string]any{"metadata": map[string]any{"name": "tenant"}, "spec": map[string]any{}},
	}
	params["namespaces"] = []any{}
	params["integrationConfig"] = map[string]any{
		"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{}, "groups": []any{}}},
	}

	gotmpl, _ := spec(tmpl)["gotemplate"].(string)
	out, err := Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
	if err != nil {
		t.Fatalf("parent gotemplate: %v", err)
	}
	children, err := Documents(out)
	if err != nil {
		t.Fatalf("decoding children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("got %d children for 3 tenants", len(children))
	}
	for _, c := range children {
		if name := meta(c)["name"]; name == parentName {
			t.Fatalf("a Tenant produced a child named %q, which is the parent TemplateInstance itself", name)
		}
	}
}

// `fromJson` exists in BOTH Helm and template-operator-v2 with different
// semantics -- it decodes a top-level array in the operator and refuses one in
// Helm -- so it is the one divergence the structural guard cannot catch: the
// function IS defined in this test engine, it just behaves differently on
// cluster. mustFromJson agrees on both sides and fails loudly on malformed
// input, so the gotemplates must use it exclusively.
//
// The child gotemplate used to cite a hack/chart-render-tests.sh for this rule.
// That script was deleted when the harness moved to Go, so for a while nothing
// enforced it at all.
func TestGotemplatesNeverUsePlainFromJson(t *testing.T) {
	comment := regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	plain := regexp.MustCompile(`\bfromJson\b`) // does not match mustFromJson

	files, err := filepath.Glob(filepath.Join("..", "..", "charts", "mto-extension-argocd", "files", "*.tpl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no gotemplates found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		// Comments explain why the plain form is avoided, so strip them first.
		for _, line := range strings.Split(string(comment.ReplaceAll(raw, nil)), "\n") {
			if plain.MatchString(line) {
				t.Errorf("%s uses plain fromJson, which behaves differently in Helm and in template-operator-v2 — use mustFromJson:\n  %s",
					filepath.Base(f), strings.TrimSpace(line))
			}
		}
	}
}
