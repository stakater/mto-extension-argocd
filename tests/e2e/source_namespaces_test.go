package e2e

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSourceNamespaces is #9 on a live cluster. Whoever can write Applications
// in a source namespace can use the AppProject that lists it, so the chart
// lists a tenant's source namespace only while that namespace exists and
// carries the tenant's label -- the same rule as a destination. Render tests
// prove what the parent decides; this proves the live object follows the
// namespace as it appears, belongs to someone else, and moves away, and that
// ArgoCD enforces the result.
func TestSourceNamespaces(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	alphaApps := tenant + "-argocd"
	betaApps := fixtureTenantSecondary + "-argocd"

	helmInstall(t, "--set", "argocd.sourceNamespaces.enabled=true")
	t.Cleanup(func() {
		unannotate(t, optInKey, sourceReposKey)
		unannotateTenant(t, fixtureTenantSecondary, optInKey)
		deleteNamespaceIfPresent(t, alphaApps)
		deleteNamespaceIfPresent(t, betaApps)
		helmUninstall(t)
	})

	// sourceRepos, so an Application under the tenant's own project fails on
	// nothing but what is under test. See TestArgoCDHonoursTheRenderedProject.
	annotate(t, sourceReposKey, `["https://github.com/argoproj/argocd-example-apps"]`)
	annotate(t, optInKey, "true")
	annotateTenant(t, fixtureTenantSecondary, optInKey, "true")
	waitFor(t, "both tenants to have AppProjects", func() bool {
		return get(t, gvkAppProject, argoNS, tenant) != nil &&
			get(t, gvkAppProject, argoNS, fixtureTenantSecondary) != nil
	})

	t.Run("a source namespace that does not exist yet is not listed", func(t *testing.T) {
		if got := sourceNamespacesOf(t, tenant); len(got) != 0 {
			t.Errorf("sourceNamespaces = %v before %s exists, want none", got, alphaApps)
		}
	})

	t.Run("it is listed once it exists with the tenant's label", func(t *testing.T) {
		if err := applyNamespace(context.Background(), alphaApps, tenant); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the source namespace to reach the live AppProject", func() bool {
			return slices.Equal(sourceNamespacesOf(t, tenant), []string{alphaApps})
		})
	})

	t.Run("ArgoCD runs an Application there under the tenant's own project", func(t *testing.T) {
		requireArgoCDWatches(t, alphaApps)
		name := "e2e-own-project"
		createApplicationIn(t, alphaApps, name, tenant, "e2e-alpha-dev")
		t.Cleanup(func() { deleteApplicationIn(t, alphaApps, name) })

		// A sync status means the controller picked the Application up. Then
		// hold a settle window: the assertion is that the refusal NEVER
		// appears, so it needs a window in which it could have.
		waitFor(t, "ArgoCD to process the Application", func() bool {
			return appSyncStatusIn(t, alphaApps, name) != ""
		})
		time.Sleep(settle)
		if c := appConditionsIn(t, alphaApps, name); refusesProject(c) {
			t.Errorf("the tenant's own source namespace was refused: %s", c)
		}
	})

	// The cross-tenant case, live: a tenant's source namespace does not let it
	// use another tenant's AppProject.
	t.Run("ArgoCD refuses an Application there under another tenant's project", func(t *testing.T) {
		requireArgoCDWatches(t, alphaApps)
		name := "e2e-other-project"
		createApplicationIn(t, alphaApps, name, fixtureTenantSecondary, "e2e-beta-dev")
		t.Cleanup(func() { deleteApplicationIn(t, alphaApps, name) })

		waitFor(t, "ArgoCD to refuse the other tenant's project", func() bool {
			return refusesProject(appConditionsIn(t, alphaApps, name))
		})
	})

	// The hole this rule closes. Before it, beta's AppProject listed
	// e2e-beta-argocd from the pattern alone, so whoever owned a namespace by
	// that name -- here alpha -- could create Applications in it under beta's
	// project and deploy into beta's namespaces.
	t.Run("a namespace that belongs to another tenant is never listed", func(t *testing.T) {
		if err := applyNamespace(context.Background(), betaApps, tenant); err != nil {
			t.Fatal(err)
		}
		// The namespace lands in alpha's list, which proves the parent
		// re-rendered after it appeared; only then is beta's absence meaningful.
		waitFor(t, "the parent to re-render with the new namespace", func() bool {
			return strings.Contains(childParam(t, tenant, "namespacesJson"), fmt.Sprintf("%q", betaApps))
		})
		if got := sourceNamespacesOf(t, fixtureTenantSecondary); len(got) != 0 {
			t.Errorf("%s's sourceNamespaces = %v; %s belongs to %s", fixtureTenantSecondary, got, betaApps, tenant)
		}
	})

	// Server-side apply has to REMOVE the field, not merely stop setting it,
	// or the previous owner's access would linger on the live object.
	t.Run("relabelling it away removes it from the live AppProject", func(t *testing.T) {
		relabelNamespace(t, alphaApps, fixtureTenantSecondary)
		waitFor(t, "the source namespace to leave the live AppProject", func() bool {
			return len(sourceNamespacesOf(t, tenant)) == 0
		})
	})
}

// refusesProject reports whether ArgoCD refused the Application's project for
// its namespace. The controller and the API server word it differently: "is
// not permitted to use project" (argo.ErrProjectNotPermitted) and "is not
// allowed to use project" (ErrApplicationNotAllowedToUseProject).
func refusesProject(conditions string) bool {
	return strings.Contains(conditions, "not permitted to use project") ||
		strings.Contains(conditions, "not allowed to use project")
}

func sourceNamespacesOf(t *testing.T, name string) []string {
	t.Helper()
	spec, _ := mustGet(t, gvkAppProject, argoNS, name).Object["spec"].(map[string]any)
	return strList(spec["sourceNamespaces"])
}

// requireArgoCDWatches skips unless ArgoCD reads Applications in ns. Without
// application.namespaces the controller ignores an Application there entirely
// -- no status at all -- and the ArgoCD subtests would only time out. make
// e2e-argocd sets "*-argocd".
func requireArgoCDWatches(t *testing.T, ns string) {
	t.Helper()
	cm := get(t, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, argoNS, "argocd-cmd-params-cm")
	data := map[string]any{}
	if cm != nil {
		data, _ = cm.Object["data"].(map[string]any)
	}
	patterns, _ := data["application.namespaces"].(string)
	for _, p := range strings.Split(patterns, ",") {
		if ok, _ := path.Match(strings.TrimSpace(p), ns); ok {
			return
		}
	}
	t.Skipf("ArgoCD does not watch %s (argocd-cmd-params-cm application.namespaces = %q)", ns, patterns)
}

// deleteNamespaceIfPresent is deleteNamespace for cleanup, where the namespace
// may never have been created because an earlier subtest failed.
func deleteNamespaceIfPresent(t *testing.T, name string) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	ns.SetName(name)
	if err := k8s.Delete(context.Background(), ns); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Errorf("deleting namespace %s: %v", name, err)
		}
		return
	}
	if err := waitForNamespaceGone(name); err != nil {
		t.Error(err)
	}
}
