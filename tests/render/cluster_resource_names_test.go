package render

import (
	"strings"
	"testing"
)

// #13. A name from a Tenant's annotation needs the same 3.3+ confirmation as a
// chart default: without it the child fails, rather than ArgoCD before 3.3
// dropping the name and allowing every Namespace in the cluster.
func TestClusterResourceNameNeedsTheVersionConfirmation(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		f := secFixture(false, false, map[string]any{
			"argocd": map[string]any{"allowClusterResourceNames": allowed},
		})
		tenants := f.Cluster["tenants"].([]any)
		tenants[0].(map[string]any)["metadata"].(map[string]any)["annotations"] = map[string]any{
			"argocd.tenantoperator.stakater.com/cluster-resource-whitelist": `[{"group":"","kind":"Namespace","name":"acme-sandbox"}]`,
		}
		out, errs := TwoStage(t, f)
		err := errs["mto-argocd-tenant-acme"]
		switch {
		case !allowed && (err == nil || !strings.Contains(err.Error(), "needs ArgoCD 3.3+")):
			t.Errorf("allowClusterResourceNames=false: child error = %v, want the 3.3+ refusal", err)
		case allowed && err != nil:
			t.Errorf("allowClusterResourceNames=true: child failed: %v", err)
		case allowed && !strings.Contains(out, "name: acme-sandbox"):
			t.Errorf("allowClusterResourceNames=true: the name was not rendered:\n%s", out)
		}
	}
}
