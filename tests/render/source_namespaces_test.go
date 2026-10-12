package render

import (
	"slices"
	"testing"
)

// #9: off by default, so no existing AppProject changes.
func TestSourceNamespacesOffByDefault(t *testing.T) {
	sp := spec(renderOne(t, secFixture(false, false, nil)))
	if v, ok := sp["sourceNamespaces"]; ok {
		t.Errorf("sourceNamespaces = %v, want absent by default", v)
	}
}

// withNamespace adds a namespace carrying tenant's label ("" for none).
func withNamespace(f Fixture, name, tenant string) Fixture {
	labels := map[string]any{}
	if tenant != "" {
		labels["stakater.com/tenant"] = tenant
	}
	f.Cluster["namespaces"] = append(f.Cluster["namespaces"].([]any),
		map[string]any{"metadata": map[string]any{"name": name, "labels": labels}})
	return f
}

// A custom pattern expands per tenant, both placeholder spellings.
func TestSourceNamespacesPattern(t *testing.T) {
	for _, p := range []string{"apps-{{ tenant }}", "apps-{{tenant}}"} {
		sp := spec(renderOne(t, withNamespace(secFixture(false, false, map[string]any{
			"argocd": map[string]any{"sourceNamespaces": map[string]any{"enabled": true, "pattern": p}},
		}), "apps-acme", "acme")))
		got, _ := sp["sourceNamespaces"].([]any)
		if want := []string{"apps-acme"}; !slices.Equal(anyStrings(got), want) {
			t.Errorf("pattern %q: sourceNamespaces = %v, want %v", p, got, want)
		}
	}
}

// Whoever can write Applications in a source namespace can use the
// AppProject, so it is listed only while it is the tenant's own: absent, or
// owned by another tenant or by none, it is left out.
func TestSourceNamespaceOnlyWhenTheTenantOwnsIt(t *testing.T) {
	on := map[string]any{"argocd": map[string]any{"sourceNamespaces": map[string]any{"enabled": true}}}
	for _, c := range []struct {
		name  string
		owner string // "-" for no such namespace
	}{
		{"absent", "-"},
		{"another tenant's", "beta"},
		{"no tenant's", ""},
	} {
		f := secFixture(false, false, on)
		if c.owner != "-" {
			f = withNamespace(f, "acme-argocd", c.owner)
		}
		if v, ok := spec(renderOne(t, f))["sourceNamespaces"]; ok {
			t.Errorf("%s acme-argocd: sourceNamespaces = %v, want absent", c.name, v)
		}
	}
}

func TestSchemaRefusesSourceNamespacesWithoutPattern(t *testing.T) {
	_, err := HelmTemplate(t, "", map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceNamespaces.pattern": ""})
	if err == nil {
		t.Fatal("an empty sourceNamespaces.pattern rendered; the schema must refuse it")
	}
}

func TestSchemaRefusesSourceNamespacesWithoutTenant(t *testing.T) {
	_, err := HelmTemplate(t, "", map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceNamespaces.pattern": "shared-apps"})
	if err == nil {
		t.Fatal("a sourceNamespaces.pattern without {{ tenant }} rendered; the schema must refuse it")
	}
}
