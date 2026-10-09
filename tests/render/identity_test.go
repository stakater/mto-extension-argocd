package render

import (
	"slices"
	"testing"
)

func roleGroups(t *testing.T, sp map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	roles, _ := sp["roles"].([]any)
	for _, r := range roles {
		rm, _ := r.(map[string]any)
		name, _ := rm["name"].(string)
		g, _ := rm["groups"].([]any)
		out[name[len("acme-"):]] = anyStrings(g)
	}
	return out
}

// #10. accessControl is the default and unchanged; both keeps it and appends
// the pattern group; groupPattern replaces it. The generated group always
// leads, so the equivalence ordering holds in every mode.
func TestIdentityModes(t *testing.T) {
	tier := map[string]string{"owner": "owners", "edit": "editors", "view": "viewers"}
	acl := map[string][]string{"owner": {"acme-admins"}, "edit": {"acme-devs"}, "view": {"acme-audit"}}
	for _, mode := range []string{"", "accessControl", "both", "groupPattern"} {
		t.Run("mode="+mode, func(t *testing.T) {
			values := map[string]any{}
			if mode != "" {
				values["identity"] = map[string]any{"mode": mode}
			}
			got := roleGroups(t, spec(renderOne(t, secFixture(false, false, values))))
			for _, role := range []string{"owner", "edit", "view"} {
				g := got[role]
				if len(g) == 0 {
					t.Fatalf("%s: no groups", role)
				}
				// The generated group leads in every mode; its hash is pinned by
				// the property test and the goldens.
				if want := "acme-" + role + "-group-"; len(g[0]) <= len(want) || g[0][:len(want)] != want {
					t.Errorf("%s: first group %q, want the generated one", role, g[0])
				}
				pattern := "tenant-acme-" + tier[role]
				hasACL := slices.Contains(g, acl[role][0])
				hasPattern := slices.Contains(g, pattern)
				switch mode {
				case "", "accessControl":
					if !hasACL || hasPattern {
						t.Errorf("%s groups %v: want the Tenant's, not the pattern's", role, g)
					}
				case "both":
					if !hasACL || !hasPattern || g[len(g)-1] != pattern {
						t.Errorf("%s groups %v: want the Tenant's, then %s last", role, g, pattern)
					}
				case "groupPattern":
					if hasACL || !hasPattern {
						t.Errorf("%s groups %v: want %s and not the Tenant's", role, g, pattern)
					}
				}
			}
		})
	}
}

func TestIdentityCustomPattern(t *testing.T) {
	got := roleGroups(t, spec(renderOne(t, secFixture(false, false, map[string]any{
		"identity": map[string]any{"mode": "groupPattern", "groupPattern": "argocd-{{tenant}}-{{tier}}"},
	}))))
	if !slices.Contains(got["view"], "argocd-acme-viewers") {
		t.Errorf("view groups %v, want argocd-acme-viewers", got["view"])
	}
}
