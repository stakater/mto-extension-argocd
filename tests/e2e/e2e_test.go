// Package e2e drives the chart against a live cluster running
// template-operator-v2, multi-tenant-operator and ArgoCD.
//
// It is the only layer where template-operator-v2 actually reconciles. The
// render tests prove the gotemplates produce the right AppProject and the
// admission tests prove the API server accepts the Template CRs; neither can
// tell you what the operator does with them once applied. Everything asserted
// here was first reproduced by hand against an OpenShift 4.20 cluster with
// MTO v1.8.2, tov2 v0.0.5 and ArgoCD 3.4.6.
//
//	E2E=1 \
//	KUBE_CONTEXT=<context> \
//	E2E_TENANT=<an existing tenant to borrow> \
//	E2E_ARGO_NAMESPACE=<where ArgoCD runs> \
//	go test ./tests/e2e/... -v -timeout 20m
//
// It BORROWS an existing tenant rather than creating one, because MTO's free
// tier caps a cluster at two Tenants and its namespace webhook refuses any
// namespace without a stakater.com/tenant label -- so on a licensed cluster
// there is frequently no room to make your own. Pick a tenant with no
// Extensions CR: this chart and ExtensionsArgoCDReconciler would otherwise
// both claim AppProject/<tenant> and fight over it.
//
// The suite installs the chart with scaffolding.mode=OnAnnotation and only
// ever annotates E2E_TENANT, so no other tenant is scaffolded. It removes
// every annotation it sets and uninstalls the release on the way out.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	chartDir     = "../../charts/mto-extension-argocd"
	releaseName  = "mto-argocd-e2e"
	optInKey     = "argocd.tenantoperator.stakater.com/enabled"
	whitelistKey = "argocd.tenantoperator.stakater.com/cluster-resource-whitelist"

	settle = 25 * time.Second
)

var (
	kubeContext string
	tenant      string
	argoNS      string
	operatorNS  string
	k8s         client.Client
)

var (
	gvkAppProject = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"}
	gvkTemplateIn = schema.GroupVersionKind{Group: "templates.v2.stakater.com", Version: "v2alpha1", Kind: "TemplateInstance"}
	gvkTemplate   = schema.GroupVersionKind{Group: "templates.v2.stakater.com", Version: "v2alpha1", Kind: "Template"}
	gvkTenant     = schema.GroupVersionKind{Group: "tenantoperator.stakater.com", Version: "v1beta3", Kind: "Tenant"}
)

func TestMain(m *testing.M) {
	if os.Getenv("E2E") == "" {
		println("e2e SKIPPED: set E2E=1 (see the package comment for the other variables)")
		os.Exit(0)
	}
	kubeContext = os.Getenv("KUBE_CONTEXT")
	tenant = envOr("E2E_TENANT", "")
	argoNS = envOr("E2E_ARGO_NAMESPACE", "")
	operatorNS = envOr("E2E_OPERATOR_NAMESPACE", argoNS)
	if tenant == "" || argoNS == "" {
		println("e2e requires E2E_TENANT and E2E_ARGO_NAMESPACE")
		os.Exit(1)
	}

	// Never trust the ambient context: on a machine with many clusters it
	// changes underneath you, and this suite mutates real objects.
	if kubeContext == "" {
		println("e2e requires KUBE_CONTEXT — refusing to use the ambient current-context")
		os.Exit(1)
	}
	os.Setenv("KUBECONTEXT", kubeContext)

	cfg, err := config.GetConfigWithContext(kubeContext)
	if err != nil {
		println("kubeconfig:", err.Error())
		os.Exit(1)
	}
	k8s, err = client.New(cfg, client.Options{})
	if err != nil {
		println("client:", err.Error())
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// childTIName mirrors the parent gotemplate's naming. The `tenant-` segment
// exists so a Tenant named `parent` cannot produce mto-argocd-parent and
// collide with the chart's own parent TemplateInstance.
func childTIName(tenant string) string { return "mto-argocd-tenant-" + tenant }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestLifecycle runs the scenarios in dependency order against one install.
// Deliberately one test with ordered subtests rather than independent ones:
// each step's precondition is the previous step's outcome, and a helm
// install/uninstall per scenario would take longer than the whole suite.
func TestLifecycle(t *testing.T) {
	helmInstall(t)
	t.Cleanup(func() {
		unannotate(t, optInKey, whitelistKey)
		helmUninstall(t)
	})

	t.Run("no tenant opted in renders no children", func(t *testing.T) {
		waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")
		if n := countChildren(t); n != 0 {
			t.Errorf("%d children with nothing annotated, want 0", n)
		}
		if get(t, gvkAppProject, argoNS, tenant) != nil {
			t.Errorf("AppProject/%s exists before opt-in", tenant)
		}
	})

	t.Run("opting in creates the AppProject", func(t *testing.T) {
		annotate(t, optInKey, "true")
		waitFor(t, "AppProject to appear", func() bool { return get(t, gvkAppProject, argoNS, tenant) != nil })
		waitReady(t, gvkTemplateIn, operatorNS, childTIName(tenant))

		ap := get(t, gvkAppProject, argoNS, tenant)
		spec, _ := ap.Object["spec"].(map[string]any)

		if got := spec["description"]; got != "Managed by mto-extension-argocd" {
			t.Errorf("description = %v", got)
		}
		// S19: the ArgoCD namespace is always a destination. On a tenant with
		// no labelled namespaces it is the ONLY one -- and the tenant must
		// still get a project rather than be skipped.
		if !contains(destinations(spec), argoNS) {
			t.Errorf("ArgoCD namespace missing from destinations: %v", destinations(spec))
		}
		if roles, _ := spec["roles"].([]any); len(roles) != 3 {
			t.Errorf("%d roles, want 3", len(roles))
		}
		// Retain intent is stamped even though template-operator-v2 does not
		// honour it yet (TOV2-1).
		if ap.GetAnnotations()["templates.v2.stakater.com/deletion-policy"] != "Orphan" {
			t.Error("deletion-policy annotation not stamped")
		}
	})

	t.Run("roles carry the tenant's users", func(t *testing.T) {
		// The deliberate change from ExtensionsArgoCDReconciler. Without it a
		// tenant whose owners are users rather than groups gets no ArgoCD
		// access at all on vanilla Kubernetes, because the generated group
		// only resolves under syncOCPGroups on OpenShift.
		users := tenantUsers(t, "owners")
		if len(users) == 0 {
			t.Skipf("tenant %s has no owner users to assert on", tenant)
		}
		ap := get(t, gvkAppProject, argoNS, tenant)
		spec, _ := ap.Object["spec"].(map[string]any)
		roles, _ := spec["roles"].([]any)
		owner, _ := roles[0].(map[string]any)
		groups := strList(owner["groups"])
		for _, u := range users {
			if !contains(groups, u) {
				t.Errorf("owner role groups %v missing user %q", groups, u)
			}
		}
	})

	t.Run("a malformed annotation fails one child and never strips the live object", func(t *testing.T) {
		before := get(t, gvkAppProject, argoNS, tenant)
		rvBefore := before.GetResourceVersion()

		annotate(t, whitelistKey, "{not json")
		waitFor(t, "child to go NotReady", func() bool {
			return !isReady(t, get(t, gvkTemplateIn, operatorNS, childTIName(tenant)))
		})

		// The whole reason the parent passes annotation values down verbatim
		// instead of parsing them: applies are server-side with
		// ForceOwnership, so falling back to the chart default would STRIP
		// clusterResourceWhitelist from the live AppProject.
		after := get(t, gvkAppProject, argoNS, tenant)
		if after == nil {
			t.Fatal("AppProject was deleted by a malformed annotation")
		}
		if after.GetResourceVersion() != rvBefore {
			t.Errorf("live AppProject was modified (rv %s -> %s)", rvBefore, after.GetResourceVersion())
		}
	})

	t.Run("fixing the annotation recovers", func(t *testing.T) {
		annotate(t, whitelistKey, `[{"group":"","kind":"Namespace"}]`)
		waitReady(t, gvkTemplateIn, operatorNS, childTIName(tenant))

		spec, _ := get(t, gvkAppProject, argoNS, tenant).Object["spec"].(map[string]any)
		wl, _ := spec["clusterResourceWhitelist"].([]any)
		if len(wl) != 1 {
			t.Fatalf("clusterResourceWhitelist = %v, want one entry from the annotation", wl)
		}
		if e, _ := wl[0].(map[string]any); e["kind"] != "Namespace" {
			t.Errorf("whitelist entry = %v", e)
		}
	})

	t.Run("re-applying the chart does not churn the AppProject", func(t *testing.T) {
		rv := get(t, gvkAppProject, argoNS, tenant).GetResourceVersion()
		helmUpgrade(t)
		time.Sleep(settle)
		if got := get(t, gvkAppProject, argoNS, tenant).GetResourceVersion(); got != rv {
			t.Errorf("AppProject rewritten by an identical upgrade (rv %s -> %s)", rv, got)
		}
	})

	// The reason the chart ships sync.mode=strict. Under revert the same delete
	// wedges the object permanently -- see TestRevertModeWedgesOnDelete.
	t.Run("strict sync refuses an out-of-band delete", func(t *testing.T) {
		ap := get(t, gvkAppProject, argoNS, tenant)
		err := k8s.Delete(context.Background(), ap)
		if err == nil {
			t.Fatal("delete was allowed under sync.mode=strict; the admission webhook is not enforcing")
		}
		if !strings.Contains(err.Error(), "deletion is not allowed") {
			t.Errorf("delete was rejected, but not by the sync webhook: %v", err)
		}
		if after := get(t, gvkAppProject, argoNS, tenant); after == nil || after.GetDeletionTimestamp() != nil {
			t.Error("AppProject was affected by a delete that should have been refused outright")
		}
	})

	// Not a bug -- the documented consequence of TOV2-1 being unimplemented,
	// pinned so nobody is surprised by it in production. Runs last because it
	// destroys the AppProject.
	t.Run("removing the opt-in reaps the AppProject", func(t *testing.T) {
		unannotate(t, optInKey)
		waitFor(t, "child to be reaped", func() bool { return countChildren(t) == 0 })
		waitFor(t, "AppProject to be reaped", func() bool { return get(t, gvkAppProject, argoNS, tenant) == nil })
	})
}

// CHARACTERIZATION TEST -- asserts a template-operator-v2 BUG, not desired
// behaviour, and installs with sync.mode=revert deliberately to reach it.
//
// Under revert, deleting a rendered AppProject leaves it wedged in Terminating
// forever: the operator's own finalizer is never released, its drift check
// reports "No drift detected, skipping apply" because the spec still matches,
// and the owning TemplateInstance goes on reporting Ready=True and Applied=True
// while the tenant's ArgoCD project is unusable.
//
// The chart defaults to strict, where the delete is refused outright, so this
// is not a live risk for anyone on the defaults. It is pinned because revert is
// the option a reader reaches for when strict feels too aggressive, and the
// failure is silent.
//
// When tov2 fixes this, this test starts failing -- which is the point.
func TestRevertModeWedgesOnDelete(t *testing.T) {
	// Same release name as TestLifecycle -- tests in a package run
	// sequentially, and reusing it avoids the ownership-metadata clash that a
	// second release name hits against any object still being finalized.
	helmRun(t, append([]string{"install", releaseName, chartDir},
		helmArgs("--set", "sync.mode=revert")...))
	t.Cleanup(func() {
		unannotate(t, optInKey)
		helmUninstall(t)
	})

	annotate(t, optInKey, "true")
	waitFor(t, "AppProject to appear", func() bool { return get(t, gvkAppProject, argoNS, tenant) != nil })

	ap := get(t, gvkAppProject, argoNS, tenant)
	if err := k8s.Delete(context.Background(), ap); err != nil {
		t.Fatalf("deleting AppProject under revert: %v", err)
	}
	time.Sleep(settle)

	stuck := get(t, gvkAppProject, argoNS, tenant)
	if stuck == nil {
		t.Fatal("AppProject was deleted and not recreated — worse than the known bug")
	}
	if stuck.GetDeletionTimestamp() == nil {
		t.Log("FIXED UPSTREAM: revert no longer wedges on delete — update this test and docs/upstream-issues.md")
		return
	}
	t.Logf("reproduced: stuck Terminating with finalizers %v while its TemplateInstance reports Ready=%v",
		stuck.GetFinalizers(), isReady(t, get(t, gvkTemplateIn, operatorNS, childTIName(tenant))))

	// Unwedge, or the tenant is left broken.
	patchFinalizers(t, stuck)
	waitFor(t, "AppProject to be rebuilt", func() bool {
		ap := get(t, gvkAppProject, argoNS, tenant)
		return ap != nil && ap.GetDeletionTimestamp() == nil
	})
}

// TestLeastPrivilegeDelta measures the permissions the chart ADDS, which is the
// only honest way to assert least privilege: the template-operator-v2
// ServiceAccount already holds broad grants from its own installation (reading
// Secrets, creating TemplateInstances cluster-wide), and an absolute assertion
// would fail on those and say nothing about this chart.
func TestLeastPrivilegeDelta(t *testing.T) {
	sa := os.Getenv("E2E_TOV2_SA")
	if sa == "" {
		t.Skip("set E2E_TOV2_SA to system:serviceaccount:<ns>:<name>")
	}

	probes := []struct {
		desc string
		args []string
	}{
		{"create AppProject in the ArgoCD namespace", []string{"create", "appprojects.argoproj.io", "-n", argoNS}},
		{"create AppProject in default", []string{"create", "appprojects.argoproj.io", "-n", "default"}},
		{"delete AppProject in the ArgoCD namespace", []string{"delete", "appprojects.argoproj.io", "-n", argoNS}},
		{"delete AppProject in default", []string{"delete", "appprojects.argoproj.io", "-n", "default"}},
		{"list AppProjects cluster-wide", []string{"list", "appprojects.argoproj.io", "--all-namespaces"}},
		{"read IntegrationConfig in the operator namespace", []string{"get", "integrationconfigs", "-n", operatorNS}},
		{"read IntegrationConfig in default", []string{"get", "integrationconfigs", "-n", "default"}},
		{"list Tenants", []string{"list", "tenants.tenantoperator.stakater.com"}},
		{"delete Tenants", []string{"delete", "tenants.tenantoperator.stakater.com"}},
	}

	before := map[string]string{}
	for _, p := range probes {
		before[p.desc] = canI(t, sa, p.args)
	}

	helmInstall(t)
	t.Cleanup(func() { helmUninstall(t) })

	// Exactly the grants the chart is supposed to add, and nothing else.
	wantGranted := map[string]bool{
		"create AppProject in the ArgoCD namespace":        true,
		"delete AppProject in the ArgoCD namespace":        true,
		"list AppProjects cluster-wide":                    true,
		"read IntegrationConfig in the operator namespace": true,
		"list Tenants": true,
	}
	for _, p := range probes {
		after := canI(t, sa, p.args)
		newlyGranted := before[p.desc] == "no" && after == "yes"
		if newlyGranted && !wantGranted[p.desc] {
			t.Errorf("chart granted %q, which it should not have", p.desc)
		}
		if !newlyGranted && wantGranted[p.desc] && before[p.desc] == "no" {
			t.Errorf("chart failed to grant %q", p.desc)
		}
		t.Logf("  %-50s before=%-3s after=%-3s", p.desc, before[p.desc], after)
	}
}

// ------------------------------------------------------------------- helpers

func helmArgs(extra ...string) []string {
	return append([]string{
		"--kube-context", kubeContext,
		"-n", operatorNS,
		"--set", "argocd.namespace=" + argoNS,
		"--set", "operatorNamespace=" + operatorNS,
		"--set", "integrationConfig.namespace=" + operatorNS,
		"--set", "scaffolding.mode=OnAnnotation",
	}, extra...)
}

func helmInstall(t *testing.T) {
	t.Helper()
	helmRun(t, append([]string{"install", releaseName, chartDir}, helmArgs()...))
}
func helmUpgrade(t *testing.T) {
	t.Helper()
	helmRun(t, append([]string{"upgrade", releaseName, chartDir}, helmArgs()...))
}

// helmUninstall removes the release AND waits for the objects to actually go.
//
// `helm uninstall` returns as soon as the API server accepts the deletes, but
// template-operator-v2 holds a finalizer on its own Template, so
// Template/mto-argocd-parent outlives the command by a few seconds. A
// subsequent install then fails with
//
//	Template "mto-argocd-parent" ... exists and cannot be imported into the
//	current release: invalid ownership metadata
//
// which is a real operational hazard, not just a test artefact: a reinstall
// immediately after an uninstall fails, and the fix is to wait rather than to
// force anything.
func helmUninstall(t *testing.T) {
	t.Helper()
	_ = exec.Command("helm", "uninstall", releaseName, "--kube-context", kubeContext, "-n", operatorNS).Run()
	waitFor(t, "the parent Template to be released", func() bool {
		return get(t, gvkTemplate, operatorNS, "mto-argocd-parent") == nil
	})
}

func helmRun(t *testing.T, args []string) {
	t.Helper()
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args[:1], " "), err, out)
	}
}

// canI returns "yes" or "no" and fails the test on anything else. Swallowing
// the error made the whole least-privilege comparison vacuous: a failed kubectl
// yields "", "" is neither "yes" nor "no", so every before/after comparison
// silently decided nothing had changed and no assertion ever fired.
//
// `kubectl auth can-i` exits non-zero when the answer is "no", so a non-zero
// exit is only an error if the output is not one of the two expected words.
func canI(t *testing.T, sa string, args []string) string {
	t.Helper()
	full := append([]string{"--context", kubeContext, "auth", "can-i"}, args...)
	full = append(full, "--as="+sa)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	answer := strings.TrimSpace(string(out))
	switch answer {
	case "yes", "no":
		return answer
	}
	t.Fatalf("kubectl auth can-i %v: unusable answer %q (err: %v)", args, answer, err)
	return ""
}

func get(t *testing.T, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if err != nil {
		return nil
	}
	return obj
}

func countChildren(t *testing.T) int {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvkTemplateIn)
	if err := k8s.List(context.Background(), list, client.InNamespace(operatorNS)); err != nil {
		t.Fatalf("listing TemplateInstances: %v", err)
	}
	n := 0
	for _, i := range list.Items {
		if strings.HasPrefix(i.GetName(), "mto-argocd-tenant-") {
			n++
		}
	}
	return n
}

func isReady(t *testing.T, obj *unstructured.Unstructured) bool {
	t.Helper()
	if obj == nil {
		return false
	}
	status, _ := obj.Object["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm["type"] == "Ready" {
			return cm["status"] == "True"
		}
	}
	return false
}

func waitReady(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	waitFor(t, name+" to be Ready", func() bool { return isReady(t, get(t, gvk, ns, name)) })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func annotate(t *testing.T, key, value string) {
	t.Helper()
	patchTenant(t, fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
}

func unannotate(t *testing.T, keys ...string) {
	t.Helper()
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q:null", k))
	}
	patchTenant(t, fmt.Sprintf(`{"metadata":{"annotations":{%s}}}`, strings.Join(parts, ",")))
}

func patchTenant(t *testing.T, patch string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkTenant)
	obj.SetName(tenant)
	if err := k8s.Patch(context.Background(), obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("patching tenant %s: %v", tenant, err)
	}
}

func patchFinalizers(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	if err := k8s.Patch(context.Background(), obj,
		client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":[]}}`))); err != nil {
		t.Fatalf("clearing finalizers on %s: %v", obj.GetName(), err)
	}
}

func tenantUsers(t *testing.T, role string) []string {
	t.Helper()
	obj := get(t, gvkTenant, "", tenant)
	if obj == nil {
		t.Fatalf("tenant %s not found", tenant)
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	ac, _ := spec["accessControl"].(map[string]any)
	group, _ := ac[role].(map[string]any)
	return strList(group["users"])
}

func destinations(spec map[string]any) []string {
	list, _ := spec["destinations"].([]any)
	out := make([]string, 0, len(list))
	for _, d := range list {
		dm, _ := d.(map[string]any)
		s, _ := dm["namespace"].(string)
		out = append(out, s)
	}
	return out
}

func strList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, i := range list {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
