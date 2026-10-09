package render

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Security defaults (#6, #7). The equivalence test proves the legacy shape is
// still reachable for migration; these tests prove the shipped defaults are not
// that shape.

const secArgoNS = "argo-cd"

// secFixture is one tenant, "acme", with two namespaces. argoLabelled puts the
// tenant label on the ArgoCD namespace too; argoPrivileged makes it match a
// privileged pattern. values are merged over the base values.
func secFixture(argoLabelled, argoPrivileged bool, values map[string]any) Fixture {
	argoLabels := map[string]any{}
	if argoLabelled {
		argoLabels["stakater.com/tenant"] = "acme"
	}
	priv := []any{}
	if argoPrivileged {
		priv = append(priv, "^"+secArgoNS+"$")
	}
	argocd := map[string]any{"namespace": secArgoNS}
	v := map[string]any{"argocd": argocd, "scaffolding": map[string]any{"mode": "Always"}}
	for k, val := range values {
		if k == "argocd" {
			for ak, av := range val.(map[string]any) {
				argocd[ak] = av
			}
			continue
		}
		v[k] = val
	}
	return Fixture{
		Values: v,
		Cluster: map[string]any{
			"tenants": []any{map[string]any{
				"metadata": map[string]any{"name": "acme"},
				"spec": map[string]any{"accessControl": map[string]any{
					"owners":  map[string]any{"groups": []any{"acme-admins"}},
					"editors": map[string]any{"groups": []any{"acme-devs"}},
					"viewers": map[string]any{"groups": []any{"acme-audit"}},
				}},
			}},
			"namespaces": []any{
				map[string]any{"metadata": map[string]any{"name": "acme-dev", "labels": map[string]any{"stakater.com/tenant": "acme"}}},
				map[string]any{"metadata": map[string]any{"name": "acme-prod", "labels": map[string]any{"stakater.com/tenant": "acme"}}},
				map[string]any{"metadata": map[string]any{"name": secArgoNS, "labels": argoLabels}},
			},
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": priv, "groups": []any{}}},
			},
		},
	}
}

func legacyValues() map[string]any {
	return map[string]any{
		"argocd": map[string]any{"allowControlPlaneDestination": true},
		"roles":  map[string]any{"profile": "extensionsCompatible"},
	}
}

// #6: by default the ArgoCD namespace is never a destination -- not appended,
// and not kept even when it carries the tenant's label and is not privileged.
// An Application in the control-plane namespace may reference any AppProject,
// so deploying there is a path to ArgoCD admin.
func TestControlPlaneIsNotADestinationByDefault(t *testing.T) {
	for _, c := range []struct {
		name           string
		labelled, priv bool
	}{
		{"unlabelled", false, false},
		{"labelled for the tenant, not privileged", true, false},
		{"privileged", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sp := spec(renderOne(t, secFixture(c.labelled, c.priv, nil)))
			dests := destinationNames(sp)
			if slices.Contains(dests, secArgoNS) {
				t.Errorf("destinations %v include the ArgoCD namespace", dests)
			}
			if want := []string{"acme-dev", "acme-prod"}; !slices.Equal(dests, want) {
				t.Errorf("destinations = %v, want %v", dests, want)
			}
		})
	}
}

// The compatibility opt-in restores the legacy controller's output exactly --
// including when the ArgoCD namespace is privileged, which is what the
// captured references show -- so a migration can match first and harden next.
func TestControlPlaneDestinationOptInIsTheLegacyShape(t *testing.T) {
	for _, c := range []struct {
		name           string
		labelled, priv bool
	}{
		{"unlabelled", false, false},
		{"labelled for the tenant", true, false},
		{"privileged", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sp := spec(renderOne(t, secFixture(c.labelled, c.priv, legacyValues())))
			if n := countOf(destinationNames(sp), secArgoNS); n != 1 {
				t.Errorf("ArgoCD namespace appears %d times, want exactly 1", n)
			}
		})
	}
}

// A tenant with no namespaces now has no destinations at all; the field must
// still render as a list, not null.
func TestEmptyDestinationsRenderAsAList(t *testing.T) {
	f := secFixture(false, false, nil)
	f.Cluster["namespaces"] = []any{}
	sp := spec(renderOne(t, f))
	d, ok := sp["destinations"].([]any)
	if !ok {
		t.Fatalf("destinations = %#v, want an empty list", sp["destinations"])
	}
	if len(d) != 0 {
		t.Errorf("destinations = %v, want none", d)
	}
}

// leastPrivilegePolicies is the role table from #7, in the order the child
// emits it.
func leastPrivilegePolicies(tenant, role string, exec, clusters bool) []string {
	type rule struct{ res, act string }
	var rules []rule
	switch role {
	case "owner":
		rules = []rule{{"applications", "*"}, {"applicationsets", "*"}, {"logs", "get"}}
		if exec {
			rules = append(rules, rule{"exec", "create"})
		}
		rules = append(rules, rule{"repositories", "*"})
		if clusters {
			rules = append(rules, rule{"clusters", "*"})
		} else {
			rules = append(rules, rule{"clusters", "get"})
		}
	case "edit":
		rules = []rule{
			{"applications", "get"}, {"applications", "sync"}, {"applications", "update"},
			{"applications", "update/*"}, {"applications", "action/*"},
			{"applicationsets", "get"}, {"logs", "get"}, {"repositories", "get"}, {"clusters", "get"},
		}
	case "view":
		rules = []rule{{"applications", "get"}, {"applicationsets", "get"}, {"logs", "get"}, {"repositories", "get"}, {"clusters", "get"}}
	}
	var out []string
	for _, r := range rules {
		out = append(out, fmt.Sprintf("p, proj:%s:%s-%s, %s, %s, %s/*, allow", tenant, tenant, role, r.res, r.act, tenant))
	}
	return out
}

func rolePolicies(t *testing.T, sp map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	roles, _ := sp["roles"].([]any)
	for _, r := range roles {
		rm, _ := r.(map[string]any)
		name, _ := rm["name"].(string)
		var ps []string
		list, _ := rm["policies"].([]any)
		for _, p := range list {
			s, _ := p.(string)
			ps = append(ps, s)
		}
		out[strings.TrimPrefix(name, "acme-")] = ps
	}
	return out
}

// #7: the shipped role table. Edit is narrower than owner; nobody gets exec or
// cluster writes unless the platform turns them on, and then only owner.
func TestLeastPrivilegeRolePolicies(t *testing.T) {
	for _, c := range []struct {
		name           string
		values         map[string]any
		exec, clusters bool
	}{
		{"defaults", nil, false, false},
		{"exec enabled", map[string]any{"roles": map[string]any{"exec": map[string]any{"enabled": true}}}, true, false},
		{"clusters enabled", map[string]any{"roles": map[string]any{"clusters": map[string]any{"enabled": true}}}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := rolePolicies(t, spec(renderOne(t, secFixture(false, false, c.values))))
			for _, role := range []string{"owner", "edit", "view"} {
				want := leastPrivilegePolicies("acme", role, c.exec, c.clusters)
				if !slices.Equal(got[role], want) {
					t.Errorf("%s policies\n got  %v\n want %v", role, got[role], want)
				}
			}
		})
	}
}

// The compatibility profile is the legacy six-line shape: `*` for owner and
// edit, `get` for view, over every project-scoped resource.
func TestExtensionsCompatibleProfileIsTheLegacyShape(t *testing.T) {
	got := rolePolicies(t, spec(renderOne(t, secFixture(false, false, legacyValues()))))
	for _, role := range []string{"owner", "edit", "view"} {
		verb := "*"
		if role == "view" {
			verb = "get"
		}
		var want []string
		for _, res := range projectScopedResources {
			want = append(want, fmt.Sprintf("p, proj:acme:acme-%s, %s, %s, acme/*, allow", role, res, verb))
		}
		if !slices.Equal(got[role], want) {
			t.Errorf("%s policies\n got  %v\n want %v", role, got[role], want)
		}
	}
}

// Edit grants nothing owner does not, and lacks Application create, delete and
// override -- the point of having two roles.
func TestEditIsAStrictSubsetOfOwner(t *testing.T) {
	got := rolePolicies(t, spec(renderOne(t, secFixture(false, false, nil))))
	norm := func(p, role string) string { return strings.Replace(p, "acme-"+role, "ROLE", 1) }
	owner := map[string]bool{}
	for _, p := range got["owner"] {
		owner[norm(p, "owner")] = true
	}
	ownerAll := func(res string) bool { return owner[fmt.Sprintf("p, proj:acme:ROLE, %s, *, acme/*, allow", res)] }
	for _, p := range got["edit"] {
		f := strings.Split(p, ", ")
		if !owner[norm(p, "edit")] && !ownerAll(f[2]) {
			t.Errorf("edit grants %q, which owner does not", p)
		}
		if f[2] == "applications" && slices.Contains([]string{"*", "create", "delete", "override"}, f[3]) {
			t.Errorf("edit grants applications %s", f[3])
		}
		if f[2] == "exec" {
			t.Errorf("edit grants exec: %q", p)
		}
	}
}

// Unknown profile values are refused at install time by the schema.
func TestSchemaRefusesUnknownRolesProfile(t *testing.T) {
	_, err := HelmTemplate(t, "", map[string]string{"argocd.namespace": "argo-cd", "roles.profile": "everything"})
	if err == nil {
		t.Fatal("roles.profile=everything rendered; the schema must refuse it")
	}
}
