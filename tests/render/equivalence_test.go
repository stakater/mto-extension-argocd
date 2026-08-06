package render

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// D3 -- cross-implementation equivalence.
//
// Every other test in this package asserts the chart against itself. This one
// asserts it against the thing it replaces: golden-appproject-pe.json is
// AppProject/pe as produced by ExtensionsArgoCDReconciler on a live OpenShift
// cluster, captured alongside the exact Tenant, Namespace set and
// IntegrationConfig that produced it. It cannot be regenerated -- ArgoCD has
// since been removed from that cluster -- which is why it lives in git.
//
// The diff must be empty except for two deltas, and both are asserted
// explicitly rather than tolerated:
//
//	description      "Managed by Extensions controller"
//	              -> "Managed by mto-extension-argocd"
//
//	roles[].groups   gains the tenant's users for that role. Derived from the
//	                 Tenant fixture, not hardcoded, so the test stays honest if
//	                 the fixture changes. For `pe` that is one user on the
//	                 owner role and nothing on edit or view.
//
// It also pins the one thing a future migration will depend on: the two resource
// lists are seeded into chart values from the IntegrationConfig's deprecated
// .spec.integrations.argocd, and the diff is only empty because that seeding is
// faithful.
const spikeFixtures = "../../docs/plans/spike-argocd-tov2/fixtures"

func TestEquivalenceWithExtensionsController(t *testing.T) {
	tenant := readJSON(t, "input-tenant-pe.json")
	ic := readJSON(t, "input-integrationconfig.json")
	want := readJSON(t, "golden-appproject-pe.json")

	var nsFixture struct {
		LabelledNamespaces []string `json:"labelledNamespaces"`
		Tenant             string   `json:"tenant"`
	}
	if err := json.Unmarshal(readFile(t, "input-namespaces-pe.json"), &nsFixture); err != nil {
		t.Fatalf("parsing namespace fixture: %v", err)
	}

	icSpec := mapAt(t, ic, "spec")
	argoIntegration := mapAt(t, mapAt(t, icSpec, "integrations"), "argocd")
	argoNS, _ := argoIntegration["namespace"].(string)

	namespaces := make([]any, 0, len(nsFixture.LabelledNamespaces))
	for _, n := range nsFixture.LabelledNamespaces {
		namespaces = append(namespaces, map[string]any{
			"metadata": map[string]any{
				"name":   n,
				"labels": map[string]any{"stakater.com/tenant": nsFixture.Tenant},
			},
		})
	}

	got := renderOne(t, Fixture{
		Values: map[string]any{
			"argocd": map[string]any{
				"namespace": argoNS,
				// Seeded from the deprecated IntegrationConfig fields, exactly
				// as the migration instructs.
				"clusterResourceWhitelist":   argoIntegration["clusterResourceWhitelist"],
				"namespaceResourceBlacklist": argoIntegration["namespaceResourceBlacklist"],
			},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants":           []any{tenant},
			"namespaces":        namespaces,
			"integrationConfig": icSpec,
		},
	})

	wantSpec := mapAt(t, want, "spec")
	gotSpec := mapAt(t, got, "spec")

	t.Run("no unexpected fields", func(t *testing.T) {
		// sourceRepos is null in the reference and absent here, which is the
		// same thing to the API server and the reason the chart ships an empty
		// pattern: a default would add a field to every existing AppProject.
		assertSameKeys(t, wantSpec, gotSpec, "sourceRepos")
	})

	t.Run("description is the one deliberate change", func(t *testing.T) {
		if wantSpec["description"] != "Managed by Extensions controller" {
			t.Fatalf("reference description changed under us: %v", wantSpec["description"])
		}
		if got, want := gotSpec["description"], "Managed by mto-extension-argocd"; got != want {
			t.Errorf("description = %v, want %v", got, want)
		}
	})

	t.Run("destinations identical", func(t *testing.T) {
		// Compared as sets: the controller's order comes from an API-server
		// LIST and carries no meaning.
		if w, g := destinations(t, wantSpec), destinations(t, gotSpec); !reflect.DeepEqual(w, g) {
			t.Errorf("destinations differ\n want %v\n  got %v", w, g)
		}
	})

	for _, field := range []string{"clusterResourceWhitelist", "namespaceResourceBlacklist"} {
		t.Run(field+" identical", func(t *testing.T) {
			if !reflect.DeepEqual(normalize(wantSpec[field]), normalize(gotSpec[field])) {
				t.Errorf("%s differs\n want %v\n  got %v", field, wantSpec[field], gotSpec[field])
			}
		})
	}

	t.Run("roles differ only by the tenant's users", func(t *testing.T) {
		usersByRole := tenantUsersByRole(t, tenant)
		wantRoles := rolesByName(t, wantSpec)
		gotRoles := rolesByName(t, gotSpec)

		if len(wantRoles) != len(gotRoles) {
			t.Fatalf("role count: want %d, got %d", len(wantRoles), len(gotRoles))
		}

		for name, wr := range wantRoles {
			gr, ok := gotRoles[name]
			if !ok {
				t.Errorf("role %s missing", name)
				continue
			}
			for _, f := range []string{"description", "policies"} {
				if !reflect.DeepEqual(normalize(wr[f]), normalize(gr[f])) {
					t.Errorf("role %s .%s differs\n want %v\n  got %v", name, f, wr[f], gr[f])
				}
			}

			// The whole delta: the same groups, in the same order, with this
			// role's users appended. Order is asserted, not just membership --
			// a reordering would show up as noise in every future diff.
			role := name[len(nsFixture.Tenant)+1:] // "pe-owner" -> "owner"
			expect := append(append([]string{}, strs(t, wr["groups"])...), usersByRole[role]...)
			if g := strs(t, gr["groups"]); !reflect.DeepEqual(expect, g) {
				t.Errorf("role %s groups\n want %v\n  got %v", name, expect, g)
			}
		}
	})
}

// renderOne runs a fixture that is expected to produce exactly one AppProject.
func renderOne(t *testing.T, f Fixture) map[string]any {
	t.Helper()
	rendered, childErrs := TwoStage(t, f)
	for name, err := range childErrs {
		t.Fatalf("child %s failed: %v", name, err)
	}
	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("decoding render: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 AppProject, got %d", len(docs))
	}
	return docs[0]
}

// tenantUsersByRole maps owner/edit/view to the Tenant's users for that role.
func tenantUsersByRole(t *testing.T, tenant map[string]any) map[string][]string {
	t.Helper()
	ac := mapAt(t, mapAt(t, tenant, "spec"), "accessControl")
	out := map[string][]string{}
	for role, key := range map[string]string{"owner": "owners", "edit": "editors", "view": "viewers"} {
		group, _ := ac[key].(map[string]any)
		out[role] = strs(t, group["users"])
	}
	return out
}

func rolesByName(t *testing.T, spec map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := spec["roles"].([]any)
	for _, r := range list {
		rm, _ := r.(map[string]any)
		name, _ := rm["name"].(string)
		out[name] = rm
	}
	if len(out) == 0 {
		t.Fatal("no roles")
	}
	return out
}

func destinations(t *testing.T, spec map[string]any) []string {
	t.Helper()
	list, _ := spec["destinations"].([]any)
	out := make([]string, 0, len(list))
	for _, d := range list {
		dm, _ := d.(map[string]any)
		ns, _ := dm["namespace"].(string)
		server, _ := dm["server"].(string)
		out = append(out, fmt.Sprintf("%s@%s", ns, server))
	}
	sort.Strings(out)
	return out
}

func assertSameKeys(t *testing.T, want, got map[string]any, ignore ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, k := range ignore {
		skip[k] = true
	}
	for k, v := range want {
		if skip[k] || v == nil {
			continue
		}
		if _, ok := got[k]; !ok {
			t.Errorf("spec.%s present in the controller's output and missing here", k)
		}
	}
	for k := range got {
		if skip[k] {
			continue
		}
		if _, ok := want[k]; !ok {
			t.Errorf("spec.%s emitted here and absent from the controller's output", k)
		}
	}
}

// normalize round-trips through JSON so values decoded from YAML and from JSON
// compare equal regardless of which numeric or map type each decoder chose.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, i := range list {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}

func mapAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	out, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping", key)
	}
	return out
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(spikeFixtures, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return b
}

func readJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(readFile(t, name), &out); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return out
}
