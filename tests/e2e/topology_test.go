package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Behaviour that only a live cluster can show: server-side apply semantics,
// object identity across a rewrite, and what the operator's watches do when the
// cluster moves underneath them.
//
// None of this is reachable from tests/render, which proves what the templates
// PRODUCE. These assert what happens to objects that already exist -- an
// AppProject someone else created, a namespace that changes owner, a field the
// chart stops setting.

func TestLiveObjectSemantics(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	// A non-empty chart default is the precondition for the "explicit []
	// removes the field" subtest: with the shipping default of [] there would
	// be nothing to remove and the assertion would pass vacuously.
	// --set-json, not --set: `--set x[0].group=` yields null, and the schema
	// requires the key to be present, so the install would be refused before
	// any of this ran.
	helmInstall(t,
		"--set-json", `argocd.clusterResourceWhitelist=[{"group":"","kind":"Namespace"}]`,
		"--set", "argocd.sourceRepos.pattern=https://github.com/acme/{{ tenant }}-gitops",
	)
	t.Cleanup(func() {
		unannotate(t, optInKey, whitelistKey)
		unannotateTenant(t, fixtureTenantSecondary, optInKey)
		helmUninstall(t)
	})
	waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")

	// MIGRATION.md's load-bearing claim, and the one nothing else can check:
	// adopting an AppProject that already exists is an in-place rewrite, not a
	// delete and recreate. If it were a recreate, every project role's
	// jwtTokens would be destroyed on migration day -- silently, because the
	// object would still be there afterwards.
	t.Run("an AppProject that already exists is adopted in place", func(t *testing.T) {
		createBareAppProject(t, fixtureTenantSecondary, "hand made, not by this chart")
		before := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary)
		uid := before.GetUID()

		annotateTenant(t, fixtureTenantSecondary, optInKey, "true")
		waitFor(t, "the hand-made AppProject to be taken over", func() bool {
			ap := get(t, gvkAppProject, argoNS, fixtureTenantSecondary)
			if ap == nil {
				return false
			}
			spec, _ := ap.Object["spec"].(map[string]any)
			return spec["description"] == "Managed by mto-extension-argocd"
		})

		after := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary)
		if after.GetUID() != uid {
			t.Errorf("adoption recreated the object: uid %s -> %s — project role tokens would be lost", uid, after.GetUID())
		}
		if _, managed := after.GetLabels()["templates.v2.stakater.com/instance"]; !managed {
			t.Error("adopted AppProject carries no ownership label")
		}
	})

	t.Run("opting the primary tenant in", func(t *testing.T) {
		annotate(t, optInKey, "true")
		waitFor(t, "AppProject to appear", func() bool { return get(t, gvkAppProject, argoNS, tenant) != nil })
		if !contains(destinationsOf(t, tenant), "e2e-alpha-prod") {
			t.Fatalf("precondition: e2e-alpha-prod missing from %v", destinationsOf(t, tenant))
		}
	})

	// Moving a namespace between tenants is the shape a cross-tenant leak
	// would take. Both halves are asserted: it has to arrive AND it has to
	// leave, because a filter that only ever adds would pass a test that
	// checked one side.
	t.Run("relabelling a namespace moves it between tenants", func(t *testing.T) {
		const moved = "e2e-alpha-prod"
		relabelNamespace(t, moved, fixtureTenantSecondary)
		t.Cleanup(func() { relabelNamespace(t, moved, tenant) })

		waitFor(t, "the namespace to arrive in the new tenant's destinations", func() bool {
			return contains(destinationsOf(t, fixtureTenantSecondary), moved)
		})
		if got := destinationsOf(t, tenant); contains(got, moved) {
			t.Errorf("%s still in the previous tenant's destinations: %v — a namespace is in two tenants at once", moved, got)
		}
	})

	// S19 says the ArgoCD namespace is always a destination. The child
	// gotemplate guards against adding it twice when it also carries the tenant
	// label -- a duplicate destination is not fatal to ArgoCD, but it is a
	// silent diff on every migration comparison.
	t.Run("the ArgoCD namespace is listed once even when labelled for a tenant", func(t *testing.T) {
		relabelNamespace(t, argoNS, tenant)
		t.Cleanup(func() { relabelNamespace(t, argoNS, "") })

		// Wait on what the PARENT saw, not on the destination list: the child
		// deduplicates, so the list looks identical before and after and there
		// would be nothing to wait for. namespacesJson is the parent's own
		// output, and it changes the moment the label lands.
		waitFor(t, "the parent to see the ArgoCD namespace as a tenant namespace", func() bool {
			return strings.Contains(childParam(t, tenant, "namespacesJson"), fmt.Sprintf("%q", argoNS))
		})
		n := 0
		for _, d := range destinationsOf(t, tenant) {
			if d == argoNS {
				n++
			}
		}
		if n != 1 {
			t.Errorf("ArgoCD namespace appears %d times in destinations: %v", n, destinationsOf(t, tenant))
		}
	})

	// The chart applies server-side with ForceOwnership. An annotation that
	// overrides a chart default with an explicit empty list therefore has to
	// REMOVE the field from the live object -- if it merely stopped setting it,
	// the previous value would linger and the tenant would keep permissions it
	// asked to give up. Render tests prove the rendered manifest omits it; only
	// this proves the live object loses it.
	t.Run("an explicit empty override strips the field from the live object", func(t *testing.T) {
		waitFor(t, "the chart default to be present first", func() bool {
			spec, _ := mustGet(t, gvkAppProject, argoNS, tenant).Object["spec"].(map[string]any)
			wl, _ := spec["clusterResourceWhitelist"].([]any)
			return len(wl) == 1
		})

		annotate(t, whitelistKey, "[]")
		waitFor(t, "clusterResourceWhitelist to be removed", func() bool {
			spec, _ := mustGet(t, gvkAppProject, argoNS, tenant).Object["spec"].(map[string]any)
			_, present := spec["clusterResourceWhitelist"]
			return !present
		})
	})

	// The one shareable form of sourceRepos: a pattern templated on the tenant
	// name. A literal shared value would let every tenant deploy from every
	// other tenant's repositories, which is why no plain default exists.
	t.Run("the sourceRepos pattern is expanded per tenant", func(t *testing.T) {
		want := "https://github.com/acme/" + tenant + "-gitops"
		spec, _ := mustGet(t, gvkAppProject, argoNS, tenant).Object["spec"].(map[string]any)
		repos := strList(spec["sourceRepos"])
		if len(repos) != 1 || repos[0] != want {
			t.Errorf("sourceRepos = %v, want [%s]", repos, want)
		}
		// The other tenant's project must carry ITS name, not this one's --
		// the whole point of templating rather than sharing a literal.
		otherSpec, _ := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).Object["spec"].(map[string]any)
		if other := strList(otherSpec["sourceRepos"]); len(other) != 1 ||
			other[0] != "https://github.com/acme/"+fixtureTenantSecondary+"-gitops" {
			t.Errorf("second tenant's sourceRepos = %v, want its own repo", other)
		}
	})

	// A values change has to reach every existing AppProject, in place. If it
	// were a recreate, a routine `helm upgrade` would destroy every tenant's
	// project role tokens -- the same failure adoption avoids, on a path people
	// take far more often.
	t.Run("a chart values change updates live AppProjects in place", func(t *testing.T) {
		uid := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).GetUID()

		// helm upgrade resets every value not re-passed, so the install's
		// whitelist rides along -- without it this upgrade would ALSO silently
		// revert clusterResourceWhitelist, and this subtest would depend on the
		// ordering of the one before it.
		helmUpgrade(t,
			"--set-json", `argocd.clusterResourceWhitelist=[{"group":"","kind":"Namespace"}]`,
			"--set-json", `argocd.namespaceResourceBlacklist=[{"group":"","kind":"ResourceQuota"}]`,
			"--set", "argocd.sourceRepos.pattern=https://github.com/acme/{{ tenant }}-gitops",
		)
		waitFor(t, "the new blacklist to reach the live object", func() bool {
			spec, _ := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).Object["spec"].(map[string]any)
			bl, _ := spec["namespaceResourceBlacklist"].([]any)
			return len(bl) == 1
		})
		if got := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).GetUID(); got != uid {
			t.Errorf("upgrade recreated the AppProject (uid %s -> %s)", uid, got)
		}
	})
}

// TestFailuresStayContained is the blast-radius suite. Every case here breaks
// something deliberately and asserts that the damage stops where the design
// says it stops -- one tenant, or nothing at all.
//
// This is the layer where "fail closed" is worth more than "fail loudly": a
// parent that cannot render must not take a working fleet down with it, because
// the fleet is what tenants are actually using.
func TestFailuresStayContained(t *testing.T) {
	requireManagedTenants(t)
	startClean(t)

	helmInstall(t)
	t.Cleanup(func() {
		unannotate(t, optInKey, purgeKey)
		unannotateTenant(t, fixtureTenantSecondary, optInKey)
		helmUninstall(t)
	})

	annotate(t, optInKey, "true")
	annotateTenant(t, fixtureTenantSecondary, optInKey, "true")
	waitFor(t, "both tenants to have AppProjects", func() bool {
		return get(t, gvkAppProject, argoNS, tenant) != nil &&
			get(t, gvkAppProject, argoNS, fixtureTenantSecondary) != nil
	})

	// "True" is not "true". Every affirmative-looking spelling is refused
	// rather than coerced, because this annotation decides whether a tenant's
	// AppProject survives deletion, and guessing wrong means guessing the
	// opposite of what the author intended.
	t.Run("a near-miss purge value fails one tenant and nothing else", func(t *testing.T) {
		otherBefore := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).GetResourceVersion()
		mineBefore := mustGet(t, gvkAppProject, argoNS, tenant).GetResourceVersion()

		annotate(t, purgeKey, "True")
		waitFor(t, "the tenant's child to go NotReady", func() bool {
			return !isReady(t, get(t, gvkTemplateIn, operatorNS, childTIName(tenant)))
		})

		if rv := mustGet(t, gvkAppProject, argoNS, tenant).GetResourceVersion(); rv != mineBefore {
			t.Errorf("the failing tenant's live AppProject was modified (rv %s -> %s)", mineBefore, rv)
		}
		if rv := mustGet(t, gvkAppProject, argoNS, fixtureTenantSecondary).GetResourceVersion(); rv != otherBefore {
			t.Errorf("another tenant's AppProject moved because THIS tenant's annotation was wrong (rv %s -> %s)", otherBefore, rv)
		}

		// The parent's roll-up is what an operator alerts on, so it has to name
		// the broken tenant and stay quiet about the healthy one.
		ready, errs := parentOutputs(t)
		if v := ready[childTIName(tenant)]; !strings.Contains(v, "False") {
			t.Errorf("parent status ready[%s] = %q, want False", childTIName(tenant), v)
		}
		if v := ready[childTIName(fixtureTenantSecondary)]; !strings.Contains(v, "True") {
			t.Errorf("parent status ready[%s] = %q, want True", childTIName(fixtureTenantSecondary), v)
		}
		if v := errs[childTIName(tenant)]; !strings.Contains(v, "on-delete-purge") {
			t.Errorf("parent status errors[%s] = %q, want the on-delete-purge message", childTIName(tenant), v)
		}
	})

	t.Run("fixing the annotation recovers that tenant", func(t *testing.T) {
		unannotate(t, purgeKey)
		waitReady(t, gvkTemplateIn, operatorNS, childTIName(tenant))
	})

	// The IntegrationConfig's privileged list is the only thing keeping one
	// tenant's namespaces out of another tenant's project. If it cannot be
	// read, rendering an empty list would be the exact inverse of safe -- so
	// the parent refuses outright, and the fleet keeps running on what it
	// already rendered.
	t.Run("an unreadable privileged list fails the parent and leaves the fleet alone", func(t *testing.T) {
		before := mustGet(t, gvkAppProject, argoNS, tenant).GetResourceVersion()

		patchIntegrationConfig(t, `{"spec":{"accessControl":{"privileged":{"namespaces":null}}}}`)
		t.Cleanup(func() {
			patchIntegrationConfig(t, `{"spec":{"accessControl":{"privileged":{"namespaces":["^default$","^openshift-*","^kube-*"]}}}}`)
			waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")
		})

		waitFor(t, "the parent to refuse to render", func() bool {
			return !isReady(t, get(t, gvkTemplateIn, operatorNS, "mto-argocd-parent"))
		})
		if ap := get(t, gvkAppProject, argoNS, tenant); ap == nil {
			t.Fatal("the fleet was wiped by an unreadable IntegrationConfig — fail-closed became fail-destructive")
		} else if ap.GetResourceVersion() != before {
			t.Errorf("live AppProject changed while the parent could not render (rv %s -> %s)", before, ap.GetResourceVersion())
		}
		if n := countChildren(t); n != 2 {
			t.Errorf("%d children survived, want 2 — a broken parent must not reap", n)
		}
	})

	// Deleting a Tenant is the other way a tenant stops being scaffolded, and
	// it is the one an operator does by accident. The child must go with it;
	// what happens to the AppProject is the retain policy's business, asserted
	// in TestLifecycle.
	t.Run("deleting a Tenant reaps its child", func(t *testing.T) {
		victim := fixtureTenantSecondary
		obj := get(t, gvkTenant, "", victim)
		if obj == nil {
			t.Fatalf("precondition: Tenant %s missing", victim)
		}
		if err := k8s.Delete(context.Background(), obj); err != nil {
			t.Fatalf("deleting Tenant %s: %v", victim, err)
		}
		t.Cleanup(func() {
			// Put it back: the fixtures are provisioned once for the whole
			// package, so a test that consumes one has to restore it.
			if err := applyTenant(context.Background(), victim); err != nil {
				t.Errorf("restoring Tenant %s: %v", victim, err)
			}
		})

		waitFor(t, "the deleted tenant's child to be reaped", func() bool {
			return get(t, gvkTemplateIn, operatorNS, childTIName(victim)) == nil
		})
		// And only that one. A parent that re-rendered the whole fleet on a
		// tenant delete would show up as the other child going too.
		if get(t, gvkTemplateIn, operatorNS, childTIName(tenant)) == nil {
			t.Error("deleting one Tenant reaped another tenant's child")
		}
	})

	// Same shape, different cause: the parameter resolves against an object
	// that does not exist at all. Recorded in docs/live-deployment.md from a
	// manual run; this is that check, automated.
	t.Run("an IntegrationConfig that does not exist fails the parent and leaves the fleet alone", func(t *testing.T) {
		uidBefore := mustGet(t, gvkAppProject, argoNS, tenant).GetUID()

		helmUpgrade(t, "--set", "integrationConfig.name=does-not-exist")
		t.Cleanup(func() {
			helmUpgrade(t)
			waitReady(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")
		})

		waitFor(t, "the parent to fail parameter resolution", func() bool {
			return !isReady(t, get(t, gvkTemplateIn, operatorNS, "mto-argocd-parent"))
		})
		ap := get(t, gvkAppProject, argoNS, tenant)
		if ap == nil {
			t.Fatal("pointing the chart at a missing IntegrationConfig deleted the fleet")
		}
		if ap.GetUID() != uidBefore {
			t.Errorf("AppProject was recreated (uid %s -> %s)", uidBefore, ap.GetUID())
		}
	})
}

// ------------------------------------------------------------------- helpers

// createBareAppProject writes the kind of AppProject a cluster already has
// before migration: no ownership labels, no chart annotations, a description
// nobody else would write.
func createBareAppProject(t *testing.T, name, description string) {
	t.Helper()
	ap := newAppProject(name)
	ap.Object["spec"] = map[string]any{
		"description":  description,
		"sourceRepos":  []any{"https://example.com/pre-existing"},
		"destinations": []any{map[string]any{"namespace": "default", "server": "https://kubernetes.default.svc"}},
	}
	if err := k8s.Create(context.Background(), ap); err != nil {
		t.Fatalf("creating a bare AppProject %s: %v", name, err)
	}
}

// relabelNamespace points a namespace at a different tenant, or removes the
// label when tenant is empty.
func relabelNamespace(t *testing.T, name, tenant string) {
	t.Helper()
	value := "null"
	if tenant != "" {
		value = fmt.Sprintf("%q", tenant)
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%s}}}`, tenantNamespaceLabel, value)
	ns := newNamespace(name)
	if err := k8s.Patch(context.Background(), ns, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("relabelling namespace %s: %v", name, err)
	}
}

func patchIntegrationConfig(t *testing.T, patch string) {
	t.Helper()
	ic := newIntegrationConfig(icName, operatorNS)
	if err := k8s.Patch(context.Background(), ic, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("patching the IntegrationConfig: %v", err)
	}
}

// parentOutputs returns the parent's ready and errors roll-ups, keyed by child
// name. Values are JSON-encoded strings -- a JSONPath output is always a list,
// and template-operator-v2 stores it encoded (TOV2-6).
func parentOutputs(t *testing.T) (ready, errs map[string]string) {
	t.Helper()
	parent := get(t, gvkTemplateIn, operatorNS, "mto-argocd-parent")
	if parent == nil {
		t.Fatal("parent TemplateInstance not found")
	}
	status, _ := parent.Object["status"].(map[string]any)
	outputs, _ := status["outputs"].(map[string]any)
	return stringMap(outputs["ready"]), stringMap(outputs["errors"])
}

func stringMap(v any) map[string]string {
	in, _ := v.(map[string]any)
	out := make(map[string]string, len(in))
	for k, val := range in {
		s, _ := val.(string)
		out[k] = s
	}
	return out
}
