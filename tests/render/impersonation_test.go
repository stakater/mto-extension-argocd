package render

import (
	"slices"
	"testing"
)

func destinationServiceAccounts(sp map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := sp["destinationServiceAccounts"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		out = append(out, m)
	}
	return out
}

// #15: off by default.
func TestImpersonationOffByDefault(t *testing.T) {
	sp := spec(renderOne(t, secFixture(false, false, nil)))
	if v, ok := sp["destinationServiceAccounts"]; ok {
		t.Errorf("destinationServiceAccounts = %v, want absent by default", v)
	}
}

// One entry per tenant namespace, in destination order, the configured
// ServiceAccount, and nothing that could match anything else: no glob, no
// catch-all, no entry for the ArgoCD namespace.
func TestImpersonationOneEntryPerTenantNamespace(t *testing.T) {
	sp := spec(renderOne(t, secFixture(false, false, map[string]any{
		"argocd": map[string]any{
			"allowControlPlaneDestination": true,
			"impersonation":                map[string]any{"enabled": true, "serviceAccount": "deployer"},
		},
	})))
	var nss []string
	for _, e := range destinationServiceAccounts(sp) {
		ns, _ := e["namespace"].(string)
		nss = append(nss, ns)
		if e["defaultServiceAccount"] != "deployer" {
			t.Errorf("%s: defaultServiceAccount = %v, want deployer", ns, e["defaultServiceAccount"])
		}
		if e["server"] != "https://kubernetes.default.svc" {
			t.Errorf("%s: server = %v", ns, e["server"])
		}
	}
	if want := []string{"acme-dev", "acme-prod"}; !slices.Equal(nss, want) {
		t.Errorf("entries for %v, want %v (never the ArgoCD namespace, never a glob)", nss, want)
	}
}
