package migrate

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// `run` is the whole point of the tool: nobody should have to hold the sequence
// in their head. It must always name the FIRST incomplete step, never a later
// one -- offering a tenant before purge is off, or before the resource lists are
// seeded, is exactly the mistake it exists to prevent.
func TestRunAdvancesOneStepAtATimeInOrder(t *testing.T) {
	// Each stage mutates the fake cluster to satisfy the previous step, so the
	// expected headline walks the checklist top to bottom.
	stages := []struct {
		name, want string
		setup      func(c *cluster)
	}{
		{"chart not installed", "install the chart", func(c *cluster) {
			if err := os.Remove(c.path("parent-template.json")); err != nil {
				t.Fatalf("removing parent template: %v", err)
			}
		}},
		{"lists not seeded", "seed the resource lists", func(c *cluster) {}},
		{"overrides not carried", "carry the per-tenant overrides", func(c *cluster) {
			c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
		}},
		{"purge still on", "turn purge off", func(c *cluster) {
			c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
			c.write("extensions.json", extensionsWithoutOverrides)
			c.write("tenants.json", tenantsWithPurgeIntentCarried)
		}},
		{"ownerrefs present", "strip legacy ownerRefs", func(c *cluster) {
			c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
			c.write("extensions.json", extensionsRetained)
		}},
		{"snapshot missing", "snapshot every AppProject", func(c *cluster) {
			c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
			c.write("extensions.json", extensionsRetained)
			c.write("appprojects.json", appProjectsNoLegacyOwner)
		}},
	}

	for _, st := range stages {
		t.Run(st.name, func(t *testing.T) {
			c := newCluster(t)
			st.setup(c)
			out, calls := c.mustRun("--state-dir", c.path("state"), "run")
			if !strings.Contains(out, st.want) {
				t.Errorf("run did not offer %q as the next step:\n%s", st.want, out)
			}
			for _, call := range calls {
				if mutating(call) {
					t.Errorf("run wrote without --apply: kubectl %s", call)
				}
			}
		})
	}
}

// The two writes `run` may perform are the two the operator cannot: AppProjects
// are generated objects with no manifest in Git. Everything touching a Tenant or
// an Extensions CR must come out as text to commit, never as a kubectl call --
// otherwise the switch controlling an AppProject's existence lands outside Git.
func TestRunNeverWritesTenantsOrExtensions(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))

	// Drive every step that --apply is allowed to perform.
	for i := 0; i < 4; i++ {
		_, calls := c.mustRun("--state-dir", c.path("state"), "run", "--apply")
		for _, call := range calls {
			if !mutating(call) {
				continue
			}
			if strings.Contains(call, "tenants.tenantoperator") ||
				strings.Contains(call, "extensions.tenantoperator") {
				t.Errorf("run --apply wrote a Git-owned object: kubectl %s", call)
			}
		}
	}
}

// A tenant with neither an Extensions CR nor an AppProject was never served by
// the controller. Counting it leaves step 7 permanently short of its total, so a
// finished migration reads as unfinished and nobody trusts the checklist.
func TestRunCountsOnlyTenantsInScope(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	c.write("extensions.json", `{"items": []}`)
	c.write("appprojects.json", `{"items": []}`)

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	// defaultTenants has four Tenants and none is now in scope.
	if !strings.Contains(out, "0 of 0") {
		t.Errorf("out-of-scope tenants were counted:\n%s", out)
	}
}

// The go/no-go gate has to be in front of the operator BEFORE the next tenant is
// offered, not after. Showing it afterwards means the next commit is already
// written by the time anyone reads it.
func TestRunShowsTheDiffBeforeOfferingTheNextTenant(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	// sos has an AppProject, is already opted in, and has no Extensions CR left,
	// so it counts as cut over -- which is what makes the diff run at all.
	c.write("extensions.json", strings.Replace(extensionsRetained,
		`{"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}},`, "", 1))
	c.write("appprojects.json", appProjectsNoLegacyOwner)
	c.mustRun("--state-dir", c.path("state"), "run", "--apply") // takes the snapshot

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	diffAt := strings.Index(out, "Comparing live AppProjects")
	nextAt := strings.Index(out, "Next — cut over") // the heading, not the checklist line
	if diffAt < 0 {
		t.Fatalf("no diff was shown with a tenant already cut over:\n%s", out)
	}
	if nextAt >= 0 && nextAt < diffAt {
		t.Errorf("the next tenant is offered at %d, before the diff at %d", nextAt, diffAt)
	}
}

// MIGRATION.md documents the same eight steps `run` prints, and a reader moves
// between them constantly. They drifted once already -- the doc grew an
// eleven-step scheme while the tool printed eight -- so the two numberings are
// pinned to each other here.
func TestRunChecklistMatchesTheRunbookHeadings(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("--state-dir", c.path("state"), "run")

	runbook, err := os.ReadFile("../../MIGRATION.md")
	if err != nil {
		t.Fatalf("reading MIGRATION.md: %v", err)
	}

	// "  [ ] 3  per-tenant overrides carried across" -> "3", "per-tenant overrides carried across"
	checklist := regexp.MustCompile(`(?m)^ *\[[ x]\] (\d)  (\S.*?)\s*$`)
	steps := checklist.FindAllStringSubmatch(out, -1)
	if len(steps) != 8 {
		t.Fatalf("expected 8 checklist lines, got %d:\n%s", len(steps), out)
	}

	for _, st := range steps {
		num, label := st[1], st[2]
		// The tenant counter is appended to step 7's label; drop it.
		label = strings.TrimSpace(regexp.MustCompile(`\s+\d+ of \d+$`).ReplaceAllString(label, ""))

		heading := regexp.MustCompile(`(?mi)^### ` + num + `\. (.+)$`).FindStringSubmatch(string(runbook))
		if heading == nil {
			t.Errorf("step %s (%q) has no `### %s.` heading in MIGRATION.md", num, label, num)
			continue
		}
		// Compare on the first few words: the doc heading may add a suffix such
		// as "-- `run --apply` does this", but it must not describe a different step.
		// Markdown backticks are formatting, not a difference in meaning.
		want := strings.ToLower(strings.Join(strings.Fields(label)[:2], " "))
		got := strings.ToLower(strings.ReplaceAll(heading[1], "`", ""))
		if !strings.Contains(got, want) {
			t.Errorf("step %s: run says %q, MIGRATION.md heading is %q", num, label, heading[1])
		}
	}
}

// Nothing writes without --apply. This is the property the whole runbook leans
// on -- every step tells the reader to review the dry run first, and that advice
// is worthless if a subcommand mutates anyway. Asserted per subcommand rather
// than once, because the guard lives in a shared helper and a call site that
// bypasses it would still look correct.
func TestDryRunWritesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"inventory"},
		{"values"},
		{"annotations"},
		{"strip-owner-refs"},
		{"cutover", "pe"},
		{"cutover", "legacy"},
	} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			c := newCluster(t)
			_, calls := c.mustRun(args...)
			for _, call := range calls {
				if mutating(call) {
					t.Errorf("dry run issued a mutating call: kubectl %s", call)
				}
			}
		})
	}
}

// The single ordering that the whole procedure turns on. Deleting an Extensions
// CR whose onDeletePurgeAppProject is true destroys the live AppProject, so the
// retain patch has to land -- and be read back -- before the delete.
func TestCutoverRetainsBeforeDeleting(t *testing.T) {
	c := newCluster(t)
	_, calls := c.mustRun("cutover", "pe", "--apply")

	patch := indexOf(calls, "patch", "extensions.tenantoperator.stakater.com", "onDeletePurgeAppProject")
	confirm := indexOf(calls, "get", "extensions.tenantoperator.stakater.com", "onDeletePurgeAppProject")
	del := indexOf(calls, "delete", "extensions.tenantoperator.stakater.com")

	if patch < 0 {
		t.Fatalf("no retain patch was issued for a CR with purge=true; calls: %v", calls)
	}
	if del < 0 {
		t.Fatalf("the Extensions CR was never deleted; calls: %v", calls)
	}
	if patch > del {
		t.Errorf("retain patch at %d, delete at %d -- deleting first destroys the AppProject", patch, del)
	}
	if confirm < 0 || confirm < patch || confirm > del {
		t.Errorf("the patch is not re-read between patch (%d) and delete (%d); a failed patch would go unnoticed", patch, del)
	}
}

// If the retain patch does not take, deleting the CR destroys the live
// AppProject. The script re-reads the field rather than trusting the patch's
// exit status, and refuses to go on -- which is the difference between a stalled
// migration and a deleted production AppProject.
func TestCutoverRefusesToDeleteWhenRetainDidNotTake(t *testing.T) {
	c := newCluster(t)
	c.write("retain-patch-fails", "")

	out, calls, err := c.run("cutover", "pe", "--apply")
	if err == nil {
		t.Fatal("cutover deleted the CR even though purge was still true")
	}
	if i := indexOf(calls, "delete", "extensions.tenantoperator.stakater.com"); i >= 0 {
		t.Errorf("the CR was deleted anyway: kubectl %s", calls[i])
	}
	if !strings.Contains(out, "Refusing to delete") {
		t.Errorf("the error should say why, got: %s", out)
	}
}

// A delete that does not complete means ExtensionsArgoCDReconciler is down --
// nothing else releases the finalizer. Opting the tenant in anyway would put the
// chart and a CR that is still Terminating on the same AppProject, and the
// obvious manual recovery (clearing the finalizer) skips the retain check.
func TestCutoverDoesNotOptInAfterAFailedDelete(t *testing.T) {
	c := newCluster(t)
	c.write("delete-hangs", "")

	out, calls, err := c.run("cutover", "pe", "--apply")
	if err == nil {
		t.Fatal("cutover reported success after the CR delete failed")
	}
	if i := indexOf(calls, "annotate", "enabled=true"); i >= 0 {
		t.Errorf("the tenant was opted in anyway: kubectl %s", calls[i])
	}
	if !strings.Contains(out, "ExtensionsArgoCDReconciler is not running") {
		t.Errorf("the error should name the likely cause, got: %s", out)
	}
}

// A cutover that leaves the child unable to render must stop, not move on to the
// next tenant. The live AppProject is untouched in that state -- a child that
// fails to render applies nothing -- so stopping is recoverable and continuing
// is not.
func TestCutoverStopsWhenTheChildNeverBecomesReady(t *testing.T) {
	c := newCluster(t)
	c.write("child-never-ready", "")

	out, calls, err := c.run("cutover", "pe", "sos", "--apply")
	if err == nil {
		t.Fatal("cutover reported success with the child not Ready")
	}
	if !strings.Contains(out, "did not become Ready") {
		t.Errorf("the error should name the cause, got: %s", out)
	}
	if i := indexOf(calls, "delete", "sos-extensions"); i >= 0 {
		t.Errorf("cutover carried on to the next tenant after a failure: kubectl %s", calls[i])
	}
}

// The opt-in has to come after the CR is gone. Annotating first puts both
// writers on the same AppProject: the chart wins under the strict webhook and
// the old controller enters a permanent error-retry loop, visible only on the
// Extensions CR.
func TestCutoverOptsInAfterDrainingTheCR(t *testing.T) {
	c := newCluster(t)
	_, calls := c.mustRun("cutover", "pe", "--apply")

	del := indexOf(calls, "delete", "extensions.tenantoperator.stakater.com")
	annotate := indexOf(calls, "annotate", "tenants.tenantoperator.stakater.com", "enabled=true")

	if annotate < 0 {
		t.Fatalf("the tenant was never opted in; calls: %v", calls)
	}
	if annotate < del {
		t.Errorf("opt-in at %d precedes the CR delete at %d -- that is the dual-writer collision", annotate, del)
	}
}

// A migration must never delete an AppProject, so the script must never be able
// to. There is no code path that should, and this makes that a property rather
// than a reading of the source.
func TestNoSubcommandEverDeletesAnAppProject(t *testing.T) {
	for _, args := range [][]string{
		{"cutover", "pe", "--apply"},
		{"cutover", "legacy", "--apply"},
		{"strip-owner-refs", "--apply"},
		{"annotations", "--apply"},
	} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			c := newCluster(t)
			_, calls := c.mustRun(args...)
			for _, call := range calls {
				if strings.HasPrefix(call, "delete ") && strings.Contains(call, "appproject") {
					t.Errorf("issued: kubectl %s", call)
				}
			}
		})
	}
}

// Stripping the legacy ownerRef must remove exactly that one. Replacing the
// whole list is the easy implementation and it severs unrelated ownership --
// here, a ConfigMap that has nothing to do with the migration.
func TestStripOwnerRefsKeepsUnrelatedOwners(t *testing.T) {
	c := newCluster(t)
	_, calls := c.mustRun("strip-owner-refs", "--apply")

	i := indexOf(calls, "patch", "appprojects.argoproj.io", "legacy")
	if i < 0 {
		t.Fatalf("AppProject/legacy was not patched; calls: %v", calls)
	}
	patch := calls[i]
	if strings.Contains(patch, "v1beta2") {
		t.Errorf("the legacy ownerRef survives the patch: %s", patch)
	}
	if !strings.Contains(patch, "keep-me") {
		t.Errorf("the unrelated ConfigMap ownerRef was dropped: %s", patch)
	}
	// AppProjects without a legacy ref must not be touched at all: a no-op write
	// still bumps resourceVersion and shows up as churn on a production object.
	for _, name := range []string{"pe", "sos"} {
		if j := indexOf(calls, "patch", "appprojects.argoproj.io", " "+name+" "); j >= 0 {
			t.Errorf("AppProject/%s has no legacy ownerRef but was patched: %s", name, calls[j])
		}
	}
}

// An Extensions CR that sets a list to [] means "override with empty", which is
// a different thing from leaving it unset. Skipping the empty one silently
// restores the chart default on that tenant.
func TestAnnotationsCarryExplicitEmptyLists(t *testing.T) {
	c := newCluster(t)
	_, calls := c.mustRun("annotations", "--apply")

	if indexOf(calls, "annotate", "pe", "namespace-resource-blacklist=[]") < 0 {
		t.Errorf("the explicit empty blacklist was not carried across; calls: %v", calls)
	}
	if indexOf(calls, "annotate", "pe", "source-repos=[\"https://github.com/acme/pe-gitops\"]") < 0 {
		t.Errorf("the sourceRepos override was not carried across; calls: %v", calls)
	}
	// onDeletePurgeAppProject has no effect yet (TOV2-1), but dropping it loses
	// the tenant's stated intent the day retain becomes enforceable.
	if indexOf(calls, "annotate", "pe", "on-delete-purge=true") < 0 {
		t.Errorf("purge=true was not carried across; calls: %v", calls)
	}
	// sos sets nothing, so it must inherit rather than be pinned to the defaults.
	if i := indexOf(calls, "annotate", " sos "); i >= 0 {
		t.Errorf("a tenant with no overrides was annotated anyway: %s", calls[i])
	}
}

// If the chart writes to a different namespace than the controller did, it
// creates a second set of AppProjects beside the live ones instead of adopting
// them. Every tenant ends up with two, and the diff in step 8 compares against
// objects nothing manages. This has to stop the run, not warn.
func TestInventoryRefusesAnArgoNamespaceMismatch(t *testing.T) {
	c := newCluster(t)
	c.write("ic.json", strings.Replace(defaultIntegrationConfig,
		`"namespace": "stakater-tenant-operator",`, `"namespace": "somewhere-else",`, 1))

	out, _, err := c.run("inventory")
	if err == nil {
		t.Fatal("inventory accepted an argocd.namespace mismatch")
	}
	if !strings.Contains(out, "mismatch") {
		t.Errorf("the error should name the problem, got: %s", out)
	}
}

// Adopting a tenant while the chart's resource lists are unset rewrites its
// AppProject without them -- silently, because losing a field is not an error.
// The chart no longer reads them from the IntegrationConfig, so the mismatch is
// the only signal there is.
func TestInventoryWarnsWhenResourceListsAreNotSeeded(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("inventory")
	if !strings.Contains(out, "resource lists do not match") {
		t.Errorf("no warning for unseeded resource lists:\n%s", out)
	}
}

// Every Extensions CR holds the finalizer that blocks removing the CRD,
// including one naming a tenant that never existed and so never produced an
// AppProject. Those are exactly the ones a tenant-by-tenant migration walks past.
func TestInventoryReportsExtensionsWithNoTenant(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("inventory")
	if !strings.Contains(out, "ghost-extensions") {
		t.Errorf("an Extensions CR naming a non-existent tenant was not reported:\n%s", out)
	}
}

// After the tenant-operator removal lands, the Extensions CRD is gone. `kubectl
// get` on a missing kind is an error, and under `set -e` that would abort the
// script -- reading as a broken cluster rather than as a finished migration.
func TestWorksAfterTheExtensionsCRDIsRemoved(t *testing.T) {
	c := newCluster(t)
	if err := os.Remove(c.path("extensions-crd-exists")); err != nil {
		t.Fatalf("removing the CRD marker: %v", err)
	}

	out, _ := c.mustRun("annotations")
	if !strings.Contains(out, "nothing to carry across") {
		t.Errorf("annotations did not degrade cleanly:\n%s", out)
	}

	out, calls := c.mustRun("cutover", "legacy", "--apply")
	if !strings.Contains(out, "nothing to drain") {
		t.Errorf("cutover did not degrade cleanly:\n%s", out)
	}
	if indexOf(calls, "annotate", "legacy", "enabled=true") < 0 {
		t.Errorf("cutover should still opt the tenant in; calls: %v", calls)
	}
}

// The opt-in annotation decides whether a tenant has an AppProject at all. Set
// out of band on a self-healing cluster, a sync can take it away again -- which
// deletes that AppProject. So a GitOps-managed Tenant has to steer the operator
// away from --apply, and this is the only place that can happen automatically.
func TestInventoryDetectsGitOpsManagedTenants(t *testing.T) {
	for _, tc := range []struct{ name, patch string }{
		{"tracking label", `{"app.kubernetes.io/instance":"tenant-config"}`},
		{"tracking annotation", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			if tc.patch != "" {
				c.write("tenants.json", strings.Replace(defaultTenants,
					`{"metadata": {"name": "pe"},`,
					`{"metadata": {"name": "pe", "labels": `+tc.patch+`},`, 1))
			} else {
				c.write("tenants.json", strings.Replace(defaultTenants,
					`{"metadata": {"name": "pe"},`,
					`{"metadata": {"name": "pe", "annotations": {"argocd.argoproj.io/tracking-id": "x:y/Tenant:/pe"}},`, 1))
			}

			out, _ := c.mustRun("inventory")
			if !strings.Contains(out, "must be a\n  COMMIT") {
				t.Errorf("a GitOps-managed Tenant was not called out:\n%s", out)
			}
			if !strings.Contains(out, "Do NOT use --apply") {
				t.Errorf("the output does not steer away from --apply:\n%s", out)
			}
		})
	}
}

// Tracking metadata is a positive signal only: its absence means "could not
// tell", not "not GitOps-managed". Staying silent there would read as an
// all-clear, so the section must always print.
func TestInventoryNeverImpliesTenantsAreUnmanaged(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("inventory")
	if !strings.Contains(out, "NOT proof they are unmanaged") {
		t.Errorf("silence on GitOps reads as an all-clear:\n%s", out)
	}
}

// An AppProject in Git is a worse problem than anything this migration handles:
// two writers for an object the chart renders, whose syncs the strict webhook
// will start denying. That has to stop the run, not warn.
func TestInventoryRefusesGitOpsManagedAppProjects(t *testing.T) {
	c := newCluster(t)
	c.write("appprojects.json", strings.Replace(defaultAppProjects,
		`{"metadata": {"name": "pe", "namespace": "stakater-tenant-operator"},`,
		`{"metadata": {"name": "pe", "namespace": "stakater-tenant-operator",
		  "annotations": {"argocd.argoproj.io/tracking-id": "x:argoproj.io/AppProject:ns/pe"}},`, 1))

	out, _, err := c.run("inventory")
	if err == nil {
		t.Fatal("inventory continued with GitOps-managed AppProjects")
	}
	if !strings.Contains(out, "STOP") {
		t.Errorf("the stop was not made obvious:\n%s", out)
	}
}

// A v1beta2 Tenant's spec.argocd block is dropped by the conversion webhook, so
// it cannot be read off the live object -- last-applied-configuration is the
// only place the original intent survives. Reporting it stops someone carrying
// dead config across as though it were live.
func TestInventoryReportsDeadV1beta2ArgoCDConfig(t *testing.T) {
	c := newCluster(t)
	lac := `{\"apiVersion\":\"tenantoperator.stakater.com/v1beta2\",\"kind\":\"Tenant\",` +
		`\"metadata\":{\"name\":\"legacy\"},\"spec\":{\"argocd\":{\"sourceRepos\":[\"git@x\"]}}}`
	c.write("tenants.json", strings.Replace(defaultTenants,
		`{"metadata": {"name": "legacy"}, "spec": {}},`,
		`{"metadata": {"name": "legacy", "annotations":
		  {"kubectl.kubernetes.io/last-applied-configuration": "`+lac+`"}}, "spec": {}},`, 1))

	out, _ := c.mustRun("inventory")
	if !strings.Contains(out, "authored as v1beta2") {
		t.Errorf("dead v1beta2 ArgoCD config was not reported:\n%s", out)
	}
	if !strings.Contains(out, "Nothing reads that block") {
		t.Errorf("the output does not say it is dead:\n%s", out)
	}
}

// A v1beta3 Tenant, or a v1beta2 one with no argocd block, is not interesting.
// A check that fires on every tenant is a check people learn to ignore.
func TestInventoryDoesNotReportHealthyTenantsAsDeadConfig(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("inventory")
	if strings.Contains(out, "authored as v1beta2") {
		t.Errorf("reported dead config on a cluster that has none:\n%s", out)
	}
}

// snapshot then diff, with nothing changed in between, must be silent. A diff
// that reports noise on an untouched cluster is a diff nobody reads on a changed
// one -- and reading it is the go/no-go gate for the whole migration.
func TestDiffIsCleanAgainstAnUnchangedCluster(t *testing.T) {
	c := newCluster(t)
	snap := c.path("snapshot")
	c.mustRun("snapshot", snap)

	out, _ := c.mustRun("diff", snap)
	if !strings.Contains(out, "0 changed, 0 new, 0 missing") {
		t.Errorf("an unchanged cluster produced a diff:\n%s", out)
	}
}

// The two deltas the migration is allowed to produce, and nothing else. This
// pins what the runbook tells the reader to expect: if the chart ever starts
// changing something else about a live AppProject, the sentence in MIGRATION.md
// stops being true and this fails.
func TestDiffShowsOnlyTheExpectedDeltasAfterCutover(t *testing.T) {
	c := newCluster(t)
	snap := c.path("snapshot")
	c.mustRun("snapshot", snap)

	// What the chart renders for pe once it owns the object.
	c.write("appprojects.json", strings.NewReplacer(
		`"description": "Managed by Extensions controller",
     "sourceRepos"`, `"description": "Managed by mto-extension-argocd",
     "sourceRepos"`,
		`"groups": ["pe-owner-group-489fab"]`, `"groups": ["pe-owner-group-489fab", "a@x.io"]`,
	).Replace(defaultAppProjects))

	out, _ := c.mustRun("diff", snap)
	if !strings.Contains(out, "1 changed, 0 new, 0 missing") {
		t.Fatalf("expected exactly one changed AppProject:\n%s", out)
	}
	for _, want := range []string{"mto-extension-argocd", "a@x.io"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diff does not show %q:\n%s", want, out)
		}
	}
}

// A vanished AppProject is the one outcome this whole procedure exists to
// prevent, so diff must exit non-zero on it rather than print it among the rest.
func TestDiffFailsWhenAnAppProjectVanishes(t *testing.T) {
	c := newCluster(t)
	snap := c.path("snapshot")
	c.mustRun("snapshot", snap)

	c.write("appprojects.json", strings.Replace(defaultAppProjects,
		`{"metadata": {"name": "sos", "namespace": "stakater-tenant-operator"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}},`,
		"", 1))

	out, _, err := c.run("diff", snap)
	if err == nil {
		t.Fatal("diff exited 0 with an AppProject missing")
	}
	if !strings.Contains(out, "GONE") {
		t.Errorf("the missing AppProject was not called out:\n%s", out)
	}
}

// scaffolding.mode Always opts every tenant in at once, which collides with
// every Extensions CR still on the cluster -- the opposite of a tenant-by-tenant
// migration. There is also no per-tenant marker to set, so cutover has nothing
// to write.
func TestCutoverRefusesWithoutPerTenantOptIn(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplate(t, "scaffolding.mode=Always"))

	out, _, err := c.run("cutover", "pe")
	if err == nil {
		t.Fatal("cutover ran with scaffolding.mode=Always")
	}
	if !strings.Contains(out, "Always") && !strings.Contains(out, "per-tenant") {
		t.Errorf("the error should explain why, got: %s", out)
	}
}
