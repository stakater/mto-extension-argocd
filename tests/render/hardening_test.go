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
	if len(roles) == 0 {
		t.Fatal("rendered AppProject has no roles")
	}
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
			// A panic is NOT an acceptable way to fail closed: on cluster a
			// template ERROR fails one parent TemplateInstance (contained),
			// while a panic in the operator's renderer is a crash-looping
			// operator. An earlier version of this test had a bare recover()
			// here, which swallowed panics and passed vacuously -- the one
			// test behind the cross-tenant-exposure invariant asserting
			// nothing at all.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("render panicked instead of failing with an error: %v", r)
				}
			}()
			values, err := marshalValues(map[string]any{
				"argocd":      map[string]any{"namespace": "argo-cd"},
				"scaffolding": map[string]any{"mode": "Always"},
			})
			if err != nil {
				t.Fatalf("values: %v", err)
			}
			docs := MustHelmTemplate(t, values, nil)
			parent := Find(t, docs, "Template", parentTemplateName)
			params := paramMap(t, parent)
			for k, v := range clusterWith(tc.ic) {
				params[k] = v
			}
			gotmpl, _ := spec(parent)["gotemplate"].(string)
			out, err := Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
			if err == nil {
				t.Fatalf("render succeeded with no privileged.namespaces; destinations would include privileged namespaces:\n%s", out)
			}
			if !strings.Contains(err.Error(), "privileged.namespaces is absent or null") {
				t.Errorf("failed for the wrong reason: %v", err)
			}
		})
	}

	// An explicit null is nastier than absence: hasKey is TRUE for a key
	// holding null, and `| default list` would quietly turn the null into []
	// -- privileged filtering disabled for every tenant at once, from a shape
	// `kubectl edit` can produce. This is the fail-open the guard must catch.
	t.Run("privileged with null namespaces", func(t *testing.T) {
		values, err := marshalValues(map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		})
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		docs := MustHelmTemplate(t, values, nil)
		parent := Find(t, docs, "Template", parentTemplateName)
		params := paramMap(t, parent)
		for k, v := range clusterWith(map[string]any{
			"accessControl": map[string]any{"privileged": map[string]any{
				"namespaces": nil, "groups": []any{"admins"}}},
		}) {
			params[k] = v
		}
		gotmpl, _ := spec(parent)["gotemplate"].(string)
		out, err := Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
		if err == nil {
			t.Fatalf("render succeeded with privileged.namespaces: null; filtering would be silently disabled:\n%s", out)
		}
		if !strings.Contains(err.Error(), "privileged.namespaces is absent or null") {
			t.Errorf("failed for the wrong reason: %v", err)
		}
	})

	// accessControl explicitly null must reach the SAME actionable message,
	// not a Go type-assertion error from dig needing a map at every level.
	t.Run("null accessControl", func(t *testing.T) {
		values, err := marshalValues(map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		})
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		docs := MustHelmTemplate(t, values, nil)
		parent := Find(t, docs, "Template", parentTemplateName)
		params := paramMap(t, parent)
		for k, v := range clusterWith(map[string]any{"accessControl": nil}) {
			params[k] = v
		}
		gotmpl, _ := spec(parent)["gotemplate"].(string)
		_, err = Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
		if err == nil {
			t.Fatal("render succeeded with accessControl: null")
		}
		if !strings.Contains(err.Error(), "privileged.namespaces is absent or null") {
			t.Errorf("null accessControl should reach the actionable guard, not a type error: %v", err)
		}
	})

	// An uncompilable pattern must fail too. sprig's regexMatch discards the
	// compile error, so a typo in the cluster's IntegrationConfig would stop
	// filtering for every tenant at once.
	t.Run("uncompilable regex", func(t *testing.T) {
		values, err := marshalValues(map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		})
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		docs := MustHelmTemplate(t, values, nil)
		parent := Find(t, docs, "Template", parentTemplateName)
		params := paramMap(t, parent)
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

	// An empty-string pattern is a valid empty regex matching EVERY namespace,
	// so the uncompilable-regex guard misses it and it would strip all
	// destinations from every tenant -- the parent must refuse it. ".*" is the
	// same match-all but a legitimate deny-all, so it must still render.
	t.Run("empty privileged pattern", func(t *testing.T) {
		values, err := marshalValues(map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		})
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		docs := MustHelmTemplate(t, values, nil)
		parent := Find(t, docs, "Template", parentTemplateName)
		gotmpl, _ := spec(parent)["gotemplate"].(string)

		render := func(patterns []any) (string, error) {
			params := paramMap(t, parent)
			for k, v := range clusterWith(map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{
					"namespaces": patterns, "groups": []any{}}},
			}) {
				params[k] = v
			}
			return Gotemplate(gotmpl, params, Instance{Name: parentTemplateName, Namespace: "ns"})
		}

		if out, err := render([]any{"^kube-", ""}); err == nil {
			t.Fatalf("an empty privileged pattern rendered successfully:\n%s", out)
		}
		if _, err := render([]any{".*"}); err != nil {
			t.Fatalf(`".*" is a legitimate deny-all and must render, but failed: %v`, err)
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

// The namespace side of the same survival property: the parent LISTs every
// Namespace in the cluster, so one Namespace with a shape the template cannot
// read -- labels: null, no labels key at all -- would kill the PARENT and take
// every tenant's AppProject with it. A live API server has stamped
// kubernetes.io/metadata.name on every namespace since 1.22, so the truly
// label-less shapes arrive through fixtures and conversion edges -- but they
// are representable in the LIST the parameter resolves, and dying on one
// would kill every tenant.
func TestParentSurvivesEveryNamespaceShape(t *testing.T) {
	rendered, childErrs := TwoStage(t, Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd"},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants": []any{
				map[string]any{"metadata": map[string]any{"name": "alpha"}, "spec": map[string]any{}},
			},
			"namespaces": []any{
				map[string]any{"metadata": map[string]any{"name": "no-labels-key"}},
				map[string]any{"metadata": map[string]any{"name": "null-labels", "labels": nil}},
				map[string]any{"metadata": map[string]any{"name": "empty-labels", "labels": map[string]any{}}},
				map[string]any{"metadata": map[string]any{"name": "alpha-app", "labels": map[string]any{"stakater.com/tenant": "alpha"}}},
			},
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
	if len(docs) != 1 {
		t.Fatalf("got %d AppProjects, want 1", len(docs))
	}
	sp, _ := docs[0]["spec"].(map[string]any)
	dests := destinationNames(sp)
	// The ArgoCD namespace is not a destination by default (#6).
	want := []string{"alpha-app"}
	if len(dests) != 1 || dests[0] != want[0] {
		t.Errorf("destinations = %v, want %v: an unlabelled namespace leaked in or the labelled one was lost", dests, want)
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
	params := paramMap(t, tmpl)
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

// #29. The AppProject is named after the Tenant and applied with
// ForceOwnership, so a Tenant called `default` took over ArgoCD's built-in
// catch-all project: narrowed to one tenant, and deleted with the Tenant under
// purge. A reserved name must fail its own child -- which applies nothing --
// and leave every other tenant alone. `default` stays reserved when the value
// lists other names, so it cannot be configured away.
func TestTenantCannotTakeOverAReservedAppProject(t *testing.T) {
	f := Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": "argo-cd", "reservedProjectNames": []any{"platform"}},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants": []any{
				map[string]any{"metadata": map[string]any{"name": "default"}, "spec": map[string]any{}},
				map[string]any{"metadata": map[string]any{"name": "platform"}, "spec": map[string]any{}},
				map[string]any{"metadata": map[string]any{"name": "acme"}, "spec": map[string]any{}},
			},
			"namespaces": []any{},
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{}, "groups": []any{}}},
			},
		},
		ExpectChildErrors: map[string]string{
			"mto-argocd-tenant-default":  `AppProject name "default" is reserved`,
			"mto-argocd-tenant-platform": `AppProject name "platform" is reserved`,
		},
	}
	out, errs := TwoStage(t, f)
	AssertChildErrors(t, f, errs)
	docs, err := Documents(out)
	if err != nil {
		t.Fatalf("decoding AppProjects: %v", err)
	}
	if len(docs) != 1 || meta(docs[0])["name"] != "acme" {
		t.Errorf("rendered %d AppProjects, want only acme's:\n%s", len(docs), out)
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
	// No trailing \b: the pattern must also catch fromJsonArray, which does
	// not exist in template-operator-v2 at all. mustFromJson is safe from the
	// leading \b for the right reason -- lowercase "fromJson" never appears
	// inside it (the F is capital), so the word boundary sits before "must".
	plain := regexp.MustCompile(`\bfromJson`)

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
