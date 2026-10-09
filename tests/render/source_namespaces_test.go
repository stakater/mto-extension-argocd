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

// A custom pattern expands per tenant, both placeholder spellings.
func TestSourceNamespacesPattern(t *testing.T) {
	for _, p := range []string{"apps-{{ tenant }}", "apps-{{tenant}}"} {
		sp := spec(renderOne(t, secFixture(false, false, map[string]any{
			"argocd": map[string]any{"sourceNamespaces": map[string]any{"enabled": true, "pattern": p}},
		})))
		got, _ := sp["sourceNamespaces"].([]any)
		if want := []string{"apps-acme"}; !slices.Equal(anyStrings(got), want) {
			t.Errorf("pattern %q: sourceNamespaces = %v, want %v", p, got, want)
		}
	}
}

func TestSchemaRefusesSourceNamespacesWithoutPattern(t *testing.T) {
	_, err := HelmTemplate(t, "", map[string]string{"argocd.namespace": "argo-cd", "argocd.sourceNamespaces.pattern": ""})
	if err == nil {
		t.Fatal("an empty sourceNamespaces.pattern rendered; the schema must refuse it")
	}
}
