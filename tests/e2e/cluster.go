package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/stakater-ab/mto-extension-argocd/tests/internal/helm"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Fixtures for the self-provisioned mode -- E2E_MANAGE_TENANTS=1, which is what
// `make e2e-kind` sets.
//
// On a licensed cluster the suite borrows a tenant because MTO's free tier caps
// a cluster at two Tenants and its namespace webhook refuses any namespace
// without a stakater.com/tenant label. The E2E cluster runs MTO's CRDs without
// MTO, so neither limit applies and the suite can build the exact shapes it
// wants to assert on -- including the two the borrowed-tenant mode can never
// reach: a namespace matching a privileged pattern, and a tenant with no
// namespaces at all.
//
// Everything here is created before the tests run and removed afterwards; a
// leftover from a killed run is adopted rather than duplicated, so re-running
// against a still-up cluster works.
//
// Scenario tags like S19 and S21 refer to docs/plans/TASKS-argocd-extension.md.

const (
	// The primary tenant. Every test that needs one opted-in tenant uses this.
	fixtureTenant = "e2e-alpha"
	// A second tenant, provisioned WITHOUT the opt-in annotation. It exists to
	// prove that scaffolding is a filter -- TestManagedFleet relies on it staying
	// un-annotated, while the topology tests annotate it and remove the
	// annotation again on their way out.
	fixtureTenantSecondary = "e2e-beta"
	// A tenant with no namespaces at all (S21). Its AppProject must still be
	// created, with the ArgoCD namespace as its only destination.
	fixtureTenantEmpty = "e2e-gamma"

	// The IntegrationConfig hack/e2e/integrationconfig.yaml creates, which is
	// also the chart's default name. Tests that break it on purpose patch this
	// object and put it back.
	icName = "tenant-operator-config"

	// Matches ^openshift-* in hack/e2e/integrationconfig.yaml while carrying
	// the tenant label, which is the one shape MTO's managed-prefix webhook
	// makes impossible to create on a real cluster.
	fixturePrivilegedNS = "openshift-e2e-alpha"
)

// fixtureNamespaces is every namespace the suite creates, and which tenant it
// is labelled for.
var fixtureNamespaces = map[string]string{
	"e2e-alpha-dev":        fixtureTenant,
	"e2e-alpha-prod":       fixtureTenant,
	fixturePrivilegedNS:    fixtureTenant,
	"e2e-beta-dev":         fixtureTenantSecondary,
	"e2e-unlabelled-noise": "", // no tenant label: must reach nobody's destinations
}

// expectedDestinations is what fixtureTenant's AppProject should list, in no
// particular order: its two ordinary namespaces plus the ArgoCD namespace,
// which is always a destination (S19). The privileged one is absent, which is
// the whole point of the fixture.
func expectedDestinations() []string {
	return []string{"e2e-alpha-dev", "e2e-alpha-prod", argoNS}
}

// allFixtureTenants is every tenant the suite owns, in a stable order. Only
// meaningful in managed mode, where TestMain guarantees tenant==fixtureTenant.
func allFixtureTenants() []string {
	return []string{fixtureTenant, fixtureTenantSecondary, fixtureTenantEmpty}
}

func provisionFixtures() error {
	ctx := context.Background()

	if err := resetCluster(ctx); err != nil {
		return err
	}

	for _, name := range []string{fixtureTenant, fixtureTenantSecondary, fixtureTenantEmpty} {
		if err := applyTenant(ctx, name); err != nil {
			return fmt.Errorf("tenant %s: %w", name, err)
		}
	}
	for ns, owner := range fixtureNamespaces {
		if err := applyNamespace(ctx, ns, owner); err != nil {
			return fmt.Errorf("namespace %s: %w", ns, err)
		}
	}
	return nil
}

// teardownFixtures is best-effort: the suite has already reported by the time
// it runs, and a namespace that is slow to finalize must not turn a green run
// red. Deleting the Tenants first also removes the last opt-in annotation, so
// template-operator-v2 reaps whatever the tests left behind.
//
// Best-effort still prints. This also runs from TestMain's provisioning-failure
// path, and an operator staring at a broken cluster deserves to know what was
// left behind rather than a silent shrug.
func teardownFixtures() {
	ctx := context.Background()
	for _, name := range []string{fixtureTenant, fixtureTenantSecondary, fixtureTenantEmpty} {
		t := &unstructured.Unstructured{}
		t.SetGroupVersionKind(gvkTenant)
		t.SetName(name)
		if err := k8s.Delete(ctx, t); err != nil && !apierrors.IsNotFound(err) {
			fmt.Printf("teardown: deleting Tenant %s: %v\n", name, err)
		}
	}
	for ns := range fixtureNamespaces {
		if err := k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsNotFound(err) {
			fmt.Printf("teardown: deleting namespace %s: %v\n", ns, err)
		}
	}
	if err := releaseFixtureAppProjects(ctx); err != nil {
		fmt.Printf("teardown: releasing fixture AppProjects: %v\n", err)
	}
}

// resetCluster puts the cluster back to "nothing installed" before the first
// test runs.
//
// A run that is killed -- ^C, a timeout, a failed assertion in a subtest whose
// parent registered the cleanup later -- leaves a release, its children, and
// whatever AppProjects those children own. The next run then fails in its first
// assertion, on state it did not create, and the real failure is two runs back.
// Every flake this suite has had that was not a genuine finding started this
// way, so the fixtures begin by clearing it rather than by hoping.
//
// Order matters and is the same one NOTES.txt gives operators: drain the
// children before removing the release, or the RBAC goes first and each child's
// finalizer blocks forever (TOV2-5).
func resetCluster(ctx context.Context) error {
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(gvkTemplateIn)
	err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "mto-argocd-parent"}, parent)
	if err == nil {
		fmt.Println("clearing an install left behind by an earlier run")
		if err := k8s.Delete(ctx, parent); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the leftover parent TemplateInstance: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("looking for a leftover parent TemplateInstance: %w", err)
	}

	if err := deleteChildTemplateInstances(ctx); err != nil {
		return err
	}
	// Best-effort: there may be no release at all, which is the normal case.
	// Any OTHER failure is the start of exactly the cascading two-runs-back
	// mess this function exists to prevent, so it is printed, not swallowed.
	// The match is helm's exact ErrReleaseNotFound text, "release: not found"
	// -- a looser "not found" would also swallow unrelated errors that merely
	// contain those words.
	if out, err := exec.Command(helm.Bin(), "uninstall", releaseName, "--kube-context", kubeContext, "-n", operatorNS).CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "release: not found") {
		fmt.Printf("uninstalling a leftover release: %v\n%s", err, out)
	}

	if err := releaseFixtureAppProjects(ctx); err != nil {
		return err
	}
	// Tenants last, and by deletion rather than by patching what is there. A
	// Tenant left over from an earlier run can carry a different accessControl
	// block or a stale opt-in annotation, and both change what the assertions
	// see without changing what the chart did -- which is worse than a failure,
	// because it looks like a chart bug.
	return deleteFixtureTenants(ctx)
}

func deleteFixtureTenants(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	for _, name := range allFixtureTenants() {
		for {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvkTenant)
			err := k8s.Get(ctx, client.ObjectKey{Name: name}, obj)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return fmt.Errorf("reading Tenant %s: %w", name, err)
			}
			delErr := k8s.Delete(ctx, obj)
			if time.Now().After(deadline) {
				return fmt.Errorf("Tenant %s could not be removed: %v", name, delErr)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

// deleteChildTemplateInstances removes every TemplateInstance under the
// mto-argocd- name prefix -- the children AND the parent itself -- and waits
// for them to go. The prefix is a naming convention, not ownership: any other
// install of this chart in the namespace would be swept too, which is fine on
// a dedicated e2e cluster and wrong anywhere else. A child whose operator can
// no longer reach its AppProject holds its finalizer for ever, so after the
// deadline the finalizer is cleared -- loudly.
//
// Delete and update errors are carried into the terminal error rather than
// discarded: a persistent webhook denial or RBAC failure otherwise burns the
// whole deadline and reports a diagnosis-free "could not be removed".
func deleteChildTemplateInstances(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvkTemplateIn)
		if err := k8s.List(ctx, list, client.InNamespace(operatorNS)); err != nil {
			return fmt.Errorf("listing leftover TemplateInstances: %w", err)
		}
		remaining := 0
		for i := range list.Items {
			item := list.Items[i]
			if !strings.HasPrefix(item.GetName(), "mto-argocd-") {
				continue
			}
			remaining++
			if item.GetDeletionTimestamp() == nil {
				if err := k8s.Delete(ctx, &item); err != nil && !apierrors.IsNotFound(err) {
					lastErr = fmt.Errorf("deleting TemplateInstance %s: %w", item.GetName(), err)
				}
				continue
			}
			if time.Now().After(deadline) && len(item.GetFinalizers()) > 0 {
				fmt.Printf("unwedging TemplateInstance %s: stuck on finalizers %v from an earlier run\n",
					item.GetName(), item.GetFinalizers())
				item.SetFinalizers(nil)
				// A conflict just means the object moved; the next loop
				// iteration re-lists and retries with a fresh resourceVersion.
				if err := k8s.Update(ctx, &item); err != nil {
					lastErr = fmt.Errorf("clearing finalizers on TemplateInstance %s: %w", item.GetName(), err)
				}
			}
		}
		if remaining == 0 {
			return nil
		}
		if time.Now().After(deadline.Add(30 * time.Second)) {
			if lastErr != nil {
				return fmt.Errorf("%d leftover TemplateInstances could not be removed; last error: %w", remaining, lastErr)
			}
			return fmt.Errorf("%d leftover TemplateInstances could not be removed", remaining)
		}
		time.Sleep(3 * time.Second)
	}
}

// releaseFixtureAppProjects deletes any AppProject left over for a fixture
// tenant and waits for it to actually go.
//
// Retained projects are the normal case now that template-operator-v2 honours
// the retain annotation: uninstalling a release RELEASES its AppProjects rather
// than deleting them, so every run inherits whatever the last one left. Without
// this, "no tenant is opted in, so there is no AppProject" passes on a fresh
// cluster and fails on the second run -- and the leftovers go on to break the
// tests that wait for a project to appear.
//
// It waits rather than firing one delete because a project still being released
// is still matched by the strict-mode webhook, which refuses the delete
// outright for as long as that lasts.
func releaseFixtureAppProjects(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for _, name := range allFixtureTenants() {
		for {
			ap := &unstructured.Unstructured{}
			ap.SetGroupVersionKind(gvkAppProject)
			err := k8s.Get(ctx, client.ObjectKey{Namespace: argoNS, Name: name}, ap)
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return fmt.Errorf("reading AppProject %s: %w", name, err)
			}
			delErr := k8s.Delete(ctx, ap)
			if time.Now().After(deadline) {
				// Last resort, and only ever for a leftover: an object stuck
				// in Terminating on a finalizer whose operator is no longer
				// managing it can never go on its own (TOV2-5). Clearing it
				// here cannot hide an in-run defect -- the tests assert
				// teardown behaviour themselves, and this only runs before the
				// first test and after the last.
				if ap.GetDeletionTimestamp() != nil && len(ap.GetFinalizers()) > 0 {
					fmt.Printf("unwedging AppProject %s: stuck Terminating on finalizers %v from an earlier run\n",
						name, ap.GetFinalizers())
					ap.SetFinalizers(nil)
					if err := k8s.Update(ctx, ap); err != nil {
						return fmt.Errorf("clearing finalizers on AppProject %s: %w", name, err)
					}
					continue
				}
				return fmt.Errorf("AppProject %s left over from a previous run and cannot be removed: %v", name, delErr)
			}
			time.Sleep(3 * time.Second)
		}
	}
	return nil
}

// applyTenant creates the Tenant. resetCluster has already removed any earlier
// one, so the AlreadyExists branch is only reachable if something else on the
// cluster owns that name -- in which case its spec is forced to the fixture's,
// because every assertion about roles reads it back.
//
// The accessControl block carries BOTH groups and users on purpose: the users
// are what this chart adds over ExtensionsArgoCDReconciler, and without a
// tenant that has some, the assertion that proves it silently passes.
func applyTenant(ctx context.Context, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkTenant)
	obj.SetName(name)
	obj.Object["spec"] = map[string]any{
		// The only required field on v1beta3. Nothing dereferences it here --
		// no MTO means no Quota controller -- but the API server rejects a
		// Tenant without it.
		"quota": "e2e-quota",
		"accessControl": map[string]any{
			"owners": map[string]any{
				"users":  []any{name + "-owner@example.com"},
				"groups": []any{name + "-owners"},
			},
			"editors": map[string]any{
				"users":  []any{name + "-editor@example.com"},
				"groups": []any{name + "-editors"},
			},
			"viewers": map[string]any{
				"users": []any{name + "-viewer@example.com"},
			},
		},
	}
	err := k8s.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(gvkTenant)
		if err := k8s.Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
			return err
		}
		existing.Object["spec"] = obj.Object["spec"]
		existing.SetAnnotations(nil)
		return k8s.Update(ctx, existing)
	}
	return err
}

func applyNamespace(ctx context.Context, name, tenant string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if tenant != "" {
		ns.Labels = map[string]string{tenantNamespaceLabel: tenant}
	}
	err := k8s.Create(ctx, ns)
	if apierrors.IsAlreadyExists(err) {
		// Adopt: a previous run may have left it without the label, and the
		// destinations assertions depend on the label being right.
		existing := &corev1.Namespace{}
		if err := k8s.Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
			return err
		}
		if existing.Labels[tenantNamespaceLabel] == tenant {
			return nil
		}
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		if tenant == "" {
			delete(existing.Labels, tenantNamespaceLabel)
		} else {
			existing.Labels[tenantNamespaceLabel] = tenant
		}
		return k8s.Update(ctx, existing)
	}
	return err
}

// waitForNamespaceGone blocks until the API server has finished removing a
// namespace. Namespace deletion is asynchronous, and a test that creates a
// namespace, deletes it and then asserts on the AppProject would otherwise race
// the terminating object still being listed.
func waitForNamespaceGone(name string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &corev1.Namespace{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("namespace %s still present after 2m", name)
}

// managingTenants reports whether this run owns its fixtures. Tests that can
// only be expressed on a cluster the suite controls skip when it does not.
func managingTenants() bool { return os.Getenv("E2E_MANAGE_TENANTS") != "" }

// Object constructors. Unstructured needs its GVK set before every call, and
// spelling that out at each call site is where typos hide.

func newAppProject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkAppProject)
	obj.SetName(name)
	obj.SetNamespace(argoNS)
	return obj
}

func newNamespace(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	obj.SetName(name)
	return obj
}

func newIntegrationConfig(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkIntegrationConfig)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}
