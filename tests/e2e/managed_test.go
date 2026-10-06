package e2e

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Tests that need a cluster the suite owns.
//
// Each one asserts something the borrowed-tenant mode cannot reach at all:
// MTO's managed-prefix webhook forbids creating a namespace that matches a
// privileged pattern, its free tier leaves no room for a third tenant, and
// nobody wants a scaffolding-mode change or an ArgoCD Application landing on a
// licensed cluster with real tenants on it. They skip rather than fail there,
// and `make e2e-kind` always runs them.

var gvkApplication = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

func requireManagedTenants(t *testing.T) {
	t.Helper()
	if !managingTenants() {
		t.Skip("needs a cluster whose tenants the suite owns — run `make e2e-kind`")
	}
}

// TestManagedFleet installs once and asserts across all three fixture tenants.
// One install for the same reason TestLifecycle uses one: the install is the
// expensive part, and none of these subtests invalidates another's
// preconditions.
func TestManagedFleet(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	helmInstall(t)
	t.Cleanup(func() {
		unannotateTenant(t, fixtureTenantEmpty, optInKey)
		unannotate(t, optInKey)
		helmUninstall(t)
	})
	waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")

	annotate(t, optInKey, "true")
	annotateTenant(t, fixtureTenantEmpty, optInKey, "true")
	waitFor(t, "both opted-in tenants to get an AppProject", func() bool {
		return get(t, gvkAppProject, argoNS, tenant) != nil &&
			get(t, gvkAppProject, argoNS, fixtureTenantEmpty) != nil
	})

	// The path with a cross-tenant-exposure consequence, and the reason
	// this cluster exists. `openshift-e2e-alpha` carries the tenant's label and
	// matches ^openshift-* in the IntegrationConfig, so it must NOT be a
	// destination. On a licensed cluster MTO's webhook refuses to let you
	// create that namespace, so this was previously covered only by a golden
	// fixture: the regexes were asserted against the template, never against a
	// live IntegrationConfig read through an objectFieldRef.
	t.Run("a privileged namespace never reaches a tenant's destinations", func(t *testing.T) {
		got := destinationsOf(t, tenant)
		if contains(got, fixturePrivilegedNS) {
			t.Errorf("privileged namespace %s is a destination: %v", fixturePrivilegedNS, got)
		}
		want := expectedDestinations()
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("destinations = %v, want %v", got, want)
		}
	})

	// A tenant with no namespaces must still get a usable project rather
	// than be skipped, and the ArgoCD namespace is what makes it usable.
	t.Run("a tenant with no namespaces still gets a project", func(t *testing.T) {
		got := destinationsOf(t, fixtureTenantEmpty)
		if len(got) != 1 || got[0] != argoNS {
			t.Errorf("destinations = %v, want exactly [%s]", got, argoNS)
		}
	})

	// The parent watches Namespaces cluster-wide, so a namespace event has to
	// rewrite exactly one tenant's child. This is the claim README.md makes
	// about the three watches; here it is enforced.
	t.Run("adding and removing a namespace re-renders destinations", func(t *testing.T) {
		const added = "e2e-alpha-added"
		if err := applyNamespace(context.Background(), added, tenant); err != nil {
			t.Fatalf("creating %s: %v", added, err)
		}
		// The happy path deletes it mid-test; this is for the failing one,
		// where a leftover namespace labelled for the tenant would join its
		// destinations and fail TestManagedFleet on the NEXT run -- a failure
		// two runs removed from its cause.
		t.Cleanup(func() {
			if err := k8s.Delete(context.Background(), newNamespace(added)); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("cleaning up namespace %s: %v", added, err)
			}
		})
		waitFor(t, "the new namespace to appear in destinations", func() bool {
			return contains(destinationsOf(t, tenant), added)
		})

		// The other tenant must not have moved. Its child is rendered from the
		// same namespace list, so a filter that ignored the label would show up
		// here and nowhere else.
		if got := destinationsOf(t, fixtureTenantEmpty); len(got) != 1 {
			t.Errorf("unrelated tenant's destinations changed: %v", got)
		}

		deleteNamespace(t, added)
		waitFor(t, "the deleted namespace to leave destinations", func() bool {
			return !contains(destinationsOf(t, tenant), added)
		})
	})

	// OnAnnotation is a filter, and the cost of getting it wrong is an
	// AppProject appearing for a tenant nobody opted in -- which, on a cluster
	// where the Go controller still owns that tenant, means two writers.
	t.Run("a tenant that is not opted in gets nothing", func(t *testing.T) {
		if ap := get(t, gvkAppProject, argoNS, fixtureTenantSecondary); ap != nil {
			t.Errorf("AppProject/%s exists without an opt-in annotation", fixtureTenantSecondary)
		}
		if ti := get(t, gvkTemplateIn, operatorNS, childTIName(fixtureTenantSecondary)); ti != nil {
			t.Errorf("child TemplateInstance rendered for a tenant that is not opted in")
		}
	})

	// The role group order, read off a live object rather than a rendered file. The
	// privileged group comes from the IntegrationConfig the parent actually
	// read, so this also proves the objectFieldRef resolved the right object.
	t.Run("roles carry the generated group, the privileged group and the tenant's own", func(t *testing.T) {
		roles := rolesOf(t, tenant)
		if len(roles) != 3 {
			t.Fatalf("%d roles, want 3", len(roles))
		}
		owner, _ := roles[0].(map[string]any)
		groups := strList(owner["groups"])
		want := []string{
			// The generated group's hash suffix and content are asserted by
			// the render tests; here the count pins that it occupies index 0,
			// and the three named entries are asserted in position after it.
			"cluster-admins",
			tenant + "-owners",
			tenant + "-owner@example.com",
		}
		if len(groups) != 4 {
			t.Fatalf("owner groups = %v, want 4 entries", groups)
		}
		for i, w := range want {
			if groups[i+1] != w {
				t.Errorf("owner groups[%d] = %q, want %q (full: %v)", i+1, groups[i+1], w, groups)
			}
		}
	})
}

// TestArgoCDHonoursTheRenderedProject closes the loop that every other layer
// stops one step short of: an AppProject existing is not the same as ArgoCD
// enforcing it. Reproduced by hand once on a live OpenShift cluster; this is
// that check, automated.
func TestArgoCDHonoursTheRenderedProject(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	helmInstall(t)
	t.Cleanup(func() {
		unannotate(t, optInKey, sourceReposKey)
		helmUninstall(t)
	})

	// Without sourceRepos the project permits NO repository, and every
	// Application under it fails on the repo before the destination is even
	// considered -- which would make the negative case below pass for the
	// wrong reason. This is the shipping default; see "Before production" in
	// README.md.
	annotate(t, sourceReposKey, `["https://github.com/argoproj/argocd-example-apps"]`)
	annotate(t, optInKey, "true")
	waitFor(t, "AppProject to appear", func() bool { return get(t, gvkAppProject, argoNS, tenant) != nil })

	t.Run("an Application outside the project's destinations is refused", func(t *testing.T) {
		name := "e2e-outside"
		createApplication(t, name, tenant, "e2e-beta-dev")
		t.Cleanup(func() { deleteApplication(t, name) })

		waitFor(t, "ArgoCD to reject the destination", func() bool {
			return strings.Contains(appConditions(t, name), "do not match any of the allowed destinations")
		})
	})

	t.Run("an Application inside them is not", func(t *testing.T) {
		name := "e2e-inside"
		createApplication(t, name, tenant, "e2e-alpha-dev")
		t.Cleanup(func() { deleteApplication(t, name) })

		// Wait for the sync status specifically -- ArgoCD can surface it before
		// the project's destination verdict, so "any signal" would read the
		// conditions too early. Then hold a settle window: the assertion is
		// that the refusal condition NEVER appears, so it needs a window in
		// which it could have.
		waitFor(t, "ArgoCD to process the Application", func() bool {
			return appSyncStatus(t, name) != ""
		})
		time.Sleep(settle)
		if c := appConditions(t, name); strings.Contains(c, "allowed destinations") {
			t.Errorf("a destination inside the project was refused: %s", c)
		}
	})
}

// TestScaffoldingModeNarrowing pins what a scaffolding.mode change does to
// tenants the new mode excludes. It needs its own install because the mode is a
// chart value, and its own tenants because Always scaffolds all of them.
//
// The documented rule used to be "narrowing the mode is a delete". With
// template-operator-v2 v0.0.7+ honouring the retain annotation it is a
// RELEASE: the child TemplateInstance goes, the AppProject stays and stops
// being managed. Both are asserted -- the child always disappears, and which of
// the two things happens to the AppProject is reported, because it is a
// property of the operator build.
func TestScaffoldingModeNarrowing(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	helmInstall(t, "--set", "scaffolding.mode=Always")
	t.Cleanup(func() {
		helmUninstall(t)
		// Whatever the mode change released is unmanaged now, and would
		// otherwise be inherited by the next test -- or the next run.
		deleteAppProjects(t)
	})

	all := allFixtureTenants()
	waitFor(t, "every tenant to be scaffolded", func() bool {
		for _, name := range all {
			if get(t, gvkAppProject, argoNS, name) == nil {
				return false
			}
		}
		return true
	})
	// Nothing was annotated to get here: Always means the annotation is not
	// consulted at all.
	for _, name := range all {
		if tn := mustGet(t, gvkTenant, "", name); tn.GetAnnotations()[optInKey] != "" {
			t.Fatalf("test precondition broken: %s carries an opt-in annotation", name)
		}
	}

	helmUpgrade(t, "--set", "scaffolding.mode=OnAnnotation")
	waitFor(t, "every child to be reaped by the narrowed mode", func() bool {
		return countChildren(t) == 0
	})

	kept, deleted := 0, 0
	for _, name := range all {
		if ap := get(t, gvkAppProject, argoNS, name); ap == nil {
			deleted++
		} else {
			kept++
			if f := ap.GetFinalizers(); len(f) != 0 {
				t.Errorf("%s: released AppProject still carries finalizers %v", name, f)
			}
		}
	}
	switch {
	case kept == len(all):
		t.Logf("retain honoured: narrowing the mode released %d AppProjects without deleting them", kept)
	case deleted == len(all):
		t.Logf("retain NOT IMPLEMENTED in this build (tov2 v0.0.6 or earlier): narrowing scaffolding.mode deleted %d AppProjects", deleted)
	default:
		t.Errorf("inconsistent: %d AppProjects kept, %d deleted — the same policy produced both", kept, deleted)
	}
}

// TestScaffoldingModes walks the two modes nothing else exercises. Both are
// documented options a platform team will reach for, and both change what the
// chart GRANTS as well as what it renders -- which is why the RBAC is asserted
// here rather than only in the render tests.
func TestScaffoldingModes(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	helmInstall(t, "--set", "scaffolding.mode=OnLabel")
	t.Cleanup(func() {
		unlabelTenant(t, tenant, optInKey)
		unannotate(t, optInKey)
		helmUninstall(t)
	})
	waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")

	t.Run("OnLabel reads the label and ignores the annotation", func(t *testing.T) {
		// The annotation is the OTHER mode's switch. Setting it first proves
		// the modes are genuinely separate rather than both being consulted.
		annotate(t, optInKey, "true")
		time.Sleep(settle)
		if ap := get(t, gvkAppProject, argoNS, tenant); ap != nil {
			t.Error("the annotation opted a tenant in while the chart was in OnLabel mode")
		}

		labelTenant(t, tenant, optInKey, "true")
		waitFor(t, "the labelled tenant to be scaffolded", func() bool {
			return get(t, gvkAppProject, argoNS, tenant) != nil
		})
	})

	// Never is the kill switch. It must stop rendering AND keep the reap verbs,
	// because the children it is about to orphan need the operator to be able
	// to delete their AppProjects -- withdrawing that permission deadlocks
	// every child finalizer instead.
	t.Run("Never stops scaffolding but keeps the operator able to clean up", func(t *testing.T) {
		helmUpgrade(t, "--set", "scaffolding.mode=Never")
		waitFor(t, "every child to be reaped", func() bool { return countChildren(t) == 0 })

		// The RBAC half needs an identity to probe as. The reap assertion above
		// does not, so it runs either way rather than the whole case skipping.
		sa := os.Getenv("E2E_TOV2_SA")
		if sa == "" {
			t.Skip("set E2E_TOV2_SA to check the RBAC half as well")
		}
		if got := canI(t, sa, []string{"create", "appprojects.argoproj.io", "-n", argoNS}); got != "no" {
			t.Errorf("create on AppProjects = %q under scaffolding.mode=Never, want no", got)
		}
		for _, verb := range []string{"delete", "update"} {
			if got := canI(t, sa, []string{verb, "appprojects.argoproj.io", "-n", argoNS}); got != "yes" {
				t.Errorf("%s on AppProjects = %q under scaffolding.mode=Never, want yes — "+
					"without it every child finalizer deadlocks", verb, got)
			}
		}
	})
}

// ------------------------------------------------------------------- helpers

const sourceReposKey = "argocd.tenantoperator.stakater.com/source-repos"

func destinationsOf(t *testing.T, name string) []string {
	t.Helper()
	ap := get(t, gvkAppProject, argoNS, name)
	if ap == nil {
		t.Fatalf("AppProject/%s not found", name)
	}
	spec, _ := ap.Object["spec"].(map[string]any)
	return destinations(spec)
}

func rolesOf(t *testing.T, name string) []any {
	t.Helper()
	ap := get(t, gvkAppProject, argoNS, name)
	if ap == nil {
		t.Fatalf("AppProject/%s not found", name)
	}
	spec, _ := ap.Object["spec"].(map[string]any)
	roles, _ := spec["roles"].([]any)
	return roles
}

// startClean removes AppProjects a previous test retained. Without it a test
// that waits for a project to APPEAR would be satisfied by one that was already
// there, and would pass with the chart doing nothing at all -- the exact shape
// of silent coverage loss this repo keeps engineering against.
//
// Retained projects are the normal case now that template-operator-v2 honours
// the retain annotation: uninstalling a release RELEASES its AppProjects rather
// than deleting them, so every test after the first inherits them.
func startClean(t *testing.T) {
	t.Helper()
	deleteAppProjects(t)
}

// deleteAppProjects removes the fixture projects and waits for them to go,
// sharing one implementation with the suite's own start-of-run cleanup so both
// tolerate the same webhook window.
func deleteAppProjects(t *testing.T) {
	t.Helper()
	if err := releaseFixtureAppProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// labelTenant and unlabelTenant drive scaffolding.mode=OnLabel, whose switch is
// a label rather than an annotation. Same merge-patch shape as the annotation
// helpers, one level up the metadata tree.
func labelTenant(t *testing.T, name, key, value string) {
	t.Helper()
	patchTenant(t, name, fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, key, value))
}

func unlabelTenant(t *testing.T, name string, keys ...string) {
	t.Helper()
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q:null", k))
	}
	patchTenant(t, name, fmt.Sprintf(`{"metadata":{"labels":{%s}}}`, strings.Join(parts, ",")))
}

func deleteNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	ns.SetName(name)
	if err := k8s.Delete(context.Background(), ns); err != nil {
		t.Fatalf("deleting namespace %s: %v", name, err)
	}
	if err := waitForNamespaceGone(name); err != nil {
		t.Fatal(err)
	}
}

// createApplication builds the smallest Application ArgoCD will process. The
// repository is real but never cloned in the passing path -- what is under
// test is the project's verdict on the destination, which ArgoCD reaches
// before and independently of the repo.
func createApplication(t *testing.T, name, project, namespace string) {
	t.Helper()
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(gvkApplication)
	app.SetName(name)
	app.SetNamespace(argoNS)
	app.Object["spec"] = map[string]any{
		"project": project,
		"source": map[string]any{
			"repoURL":        "https://github.com/argoproj/argocd-example-apps",
			"path":           "guestbook",
			"targetRevision": "HEAD",
		},
		"destination": map[string]any{
			"server":    "https://kubernetes.default.svc",
			"namespace": namespace,
		},
		// Manual sync: the point is ArgoCD's verdict, not deploying anything
		// into the E2E cluster.
		"syncPolicy": map[string]any{},
	}
	if err := k8s.Create(context.Background(), app); err != nil {
		t.Fatalf("creating Application %s: %v", name, err)
	}
}

func deleteApplication(t *testing.T, name string) {
	t.Helper()
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(gvkApplication)
	app.SetName(name)
	app.SetNamespace(argoNS)
	// Logged, not fatal: a failed cleanup here surfaces on the NEXT create as
	// AlreadyExists, and this line is what explains it.
	if err := k8s.Delete(context.Background(), app); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("deleting Application %s: %v", name, err)
	}
}

// appConditions flattens the Application's conditions into one string, because
// every assertion here is "does ArgoCD say this anywhere".
func appConditions(t *testing.T, name string) string {
	t.Helper()
	app := get(t, gvkApplication, argoNS, name)
	if app == nil {
		return ""
	}
	status, _ := app.Object["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	var sb strings.Builder
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(string)
		typ, _ := cm["type"].(string)
		sb.WriteString(typ + ": " + msg + "\n")
	}
	return sb.String()
}

func appSyncStatus(t *testing.T, name string) string {
	t.Helper()
	app := get(t, gvkApplication, argoNS, name)
	if app == nil {
		return ""
	}
	status, _ := app.Object["status"].(map[string]any)
	sync, _ := status["sync"].(map[string]any)
	s, _ := sync["status"].(string)
	return s
}
