package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// between them constantly, so the two numberings are pinned to each other here.
func TestRunChecklistMatchesTheRunbook(t *testing.T) {
	c := newCluster(t)
	out, _ := c.mustRun("--state-dir", c.path("state"), "run")

	runbook, err := os.ReadFile("../../MIGRATION.md")
	if err != nil {
		t.Fatalf("reading MIGRATION.md: %v", err)
	}

	// The doc deliberately does not re-explain the eight steps -- `run` prints
	// the reason for each as it offers it. What it does carry is the checklist
	// itself, as sample output, so that is what this pins: same numbers, same
	// labels, same order, in both places.
	fromRun := checklistSteps(out)
	if len(fromRun) != 8 {
		t.Fatalf("expected 8 checklist lines from run, got %d:\n%s", len(fromRun), out)
	}
	fromDoc := checklistSteps(string(runbook))
	if len(fromDoc) != 8 {
		t.Fatalf("expected 8 checklist lines in MIGRATION.md, got %d -- the sample\n"+
			"output is how the doc stays in step with the tool, so it has to be there:\n%v",
			len(fromDoc), fromDoc)
	}

	for i := range fromRun {
		if fromRun[i] != fromDoc[i] {
			t.Errorf("checklist line %d drifted:\n  run:           %q\n  MIGRATION.md:  %q",
				i+1, fromRun[i], fromDoc[i])
		}
	}
}

// checklistSteps pulls "N  label" out of every "[ ] N  label" line, dropping the
// annotations that are not part of the label: step 7's tenant counter, and the
// "<- next" marker the doc's sample output uses to show what a real run looks
// like.
func checklistSteps(s string) []string {
	var steps []string
	line := regexp.MustCompile(`(?m)^ *\[[ x]\] (\d)  (\S.*?)\s*$`)
	noise := regexp.MustCompile(`\s+(\d+ of \d+|<- next)$`)
	for _, m := range line.FindAllStringSubmatch(s, -1) {
		label := m[2]
		for {
			trimmed := strings.TrimSpace(noise.ReplaceAllString(label, ""))
			if trimmed == label {
				break
			}
			label = trimmed
		}
		steps = append(steps, m[1]+"  "+label)
	}
	return steps
}

// scaffolding.mode OnLabel moves the per-tenant opt-in from an annotation to a
// LABEL. Reading the wrong field means no tenant ever counts as cut over, so step
// 7 never completes and `run` re-offers tenants it has already migrated. Both
// opt-in keys default to the same STRING, so nothing about the output looks wrong.
func TestRunReadsTheOptInFromTheFieldTheModeUses(t *testing.T) {
	// sos: AppProject present, CR drained, opted in. That is "cut over", and it
	// has to be counted as such under either mode.
	const optInKey = "argocd.tenantoperator.stakater.com/enabled"
	optedInByLabel := strings.Replace(defaultTenants,
		`{"metadata": {"name": "sos",
    "annotations": {"argocd.tenantoperator.stakater.com/enabled": "true"}}, "spec": {}},`,
		`{"metadata": {"name": "sos", "labels": {"`+optInKey+`": "true"}}, "spec": {}},`, 1)
	if !strings.Contains(optedInByLabel, `"labels"`) {
		t.Fatal("fixture rewrite did not take; the tenant fixture changed shape")
	}
	drained := strings.Replace(extensionsRetained,
		`{"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}},`, "", 1)

	for _, tc := range []struct {
		mode, tenants string
	}{
		{"OnAnnotation", defaultTenants}, // the baseline that always worked
		{"OnLabel", optedInByLabel},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c := newCluster(t)
			c.write("parent-template.json",
				renderParentTemplateWith(t, seededLists, "scaffolding.mode="+tc.mode))
			c.write("tenants.json", tc.tenants)
			c.write("extensions.json", drained)
			c.write("appprojects.json", appProjectsNoLegacyOwner)
			c.mustRun("--state-dir", c.path("state"), "run", "--apply") // snapshot

			out, _ := c.mustRun("--state-dir", c.path("state"), "--batch", "5", "run")
			if !regexp.MustCompile(`7  tenants cut over\s+1 of `).MatchString(out) {
				t.Errorf("a tenant opted in via %s was not counted as cut over:\n%s", tc.mode, out)
			}
			// The real symptom: it hands you a commit for a tenant already done.
			if strings.Contains(out, "Tenant manifest for sos") {
				t.Errorf("re-offered an already-migrated tenant under %s:\n%s", tc.mode, out)
			}
		})
	}
}

// The runbook's step 2 is "install with the output of `values`". If that output
// ever drifted from what the seeded check accepts -- a renamed key, different
// quoting of group: "" -- every test would still pass while every real
// migration stalled at step 2. So the output is fed back into a real chart
// render and the check asserted, closing the loop the fixtures alone cannot.
func TestValuesOutputSeedsTheChart(t *testing.T) {
	c := newCluster(t)
	valuesYAML, _ := c.mustRun("values")

	c2 := newCluster(t)
	c2.write("parent-template.json", renderParentTemplateWith(t, valuesYAML))
	out, _ := c2.mustRun("--state-dir", c2.path("state"), "run")
	if !strings.Contains(out, "[x] 2  resource lists seeded") {
		t.Errorf("a chart installed with the tool's own `values` output does not count as seeded:\n%s", out)
	}
}

// The chart no longer reads the resource lists from the IntegrationConfig, so
// this comparison is the only thing standing between a cutover and an AppProject
// silently rewritten without them. Comparing LENGTHS passes a chart carrying the
// right number of the wrong kinds -- the same data loss with none of the warning.
func TestResourceListsAreComparedByContentNotLength(t *testing.T) {
	const sameCountWrongKinds = `
argocd:
  clusterResourceWhitelist:
    - group: totally.wrong.io
      kind: NotTheRightKind
  namespaceResourceBlacklist:
    - group: ""
      kind: AlsoWrong
`
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, sameCountWrongKinds))

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	if !strings.Contains(out, "[ ] 2  resource lists seeded") {
		t.Errorf("step 2 passed with the right count of the wrong kinds:\n%s", out)
	}

	out, _ = c.mustRun("inventory")
	// The per-row MISMATCH marker is the assertion that matters: it distinguishes
	// "checked and wrong" from "checked and fine" from "never checked".
	if !strings.Contains(out, "MISMATCH") || !strings.Contains(out, "do not match the IntegrationConfig") {
		t.Errorf("inventory did not flag the mismatch:\n%s", out)
	}
	// A warning that does not say what differs sends you reading two YAML files.
	if !strings.Contains(out, "NotTheRightKind") || !strings.Contains(out, "EnvironmentProvisioner") {
		t.Errorf("the warning should name both sides of the difference:\n%s", out)
	}
}

// `diff` and `cutover` both tell you to restore an AppProject from the snapshot
// on the data-loss path. The normalised file cannot do that -- no apiVersion, no
// kind, metadata flattened, and jwtTokens deliberately stripped -- so the
// snapshot writes a second, applyable copy. The two jobs are different: one has
// to be stable to diff, the other has to be complete enough to put the object
// back, tokens included.
func TestSnapshotWritesAnApplyableCopy(t *testing.T) {
	withTokensAndLabels := strings.Replace(defaultAppProjects,
		`{"metadata": {"name": "sos", "namespace": "stakater-tenant-operator"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}},`,
		`{"apiVersion": "argoproj.io/v1alpha1", "kind": "AppProject",
   "metadata": {"name": "sos", "namespace": "stakater-tenant-operator",
     "labels": {"team": "sos"}, "uid": "u-1", "resourceVersion": "99",
     "annotations": {"kubectl.kubernetes.io/last-applied-configuration": "junk"}},
   "status": {"foo": "bar"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [],
     "roles": [{"name": "sos-owner", "jwtTokens": [{"iat": 1700000000, "id": "ci"}]}]}},`, 1)

	c := newCluster(t)
	c.write("appprojects.json", withTokensAndLabels)
	snap := c.path("snap")
	c.mustRun("snapshot", snap)

	restore, err := os.ReadFile(filepath.Join(snap, "restore", "sos.json"))
	if err != nil {
		t.Fatalf("no restorable copy written: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(restore, &got); err != nil {
		t.Fatalf("restorable copy is not JSON: %v", err)
	}
	for _, k := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if got[k] == nil {
			t.Errorf("restorable copy has no %q, so kubectl apply would reject it:\n%s", k, restore)
		}
	}
	if got["status"] != nil {
		t.Errorf("status is server-owned and must not be in a restorable copy:\n%s", restore)
	}
	md, _ := got["metadata"].(map[string]any)
	for _, k := range []string{"uid", "resourceVersion"} {
		if md[k] != nil {
			t.Errorf("metadata.%s is server-owned and must not be restored:\n%s", k, restore)
		}
	}
	if md["name"] != "sos" || md["namespace"] != argoNamespace {
		t.Errorf("name/namespace missing from the restorable copy:\n%s", restore)
	}
	if labels, _ := md["labels"].(map[string]any); labels["team"] != "sos" {
		t.Errorf("caller-owned labels must survive a restore:\n%s", restore)
	}
	if anns, _ := md["annotations"].(map[string]any); anns != nil && anns["kubectl.kubernetes.io/last-applied-configuration"] != nil {
		t.Errorf("last-applied-configuration must not be carried into a restore:\n%s", restore)
	}
	// The whole reason this file exists: tokens do not come back on their own.
	if !strings.Contains(string(restore), `"id": "ci"`) {
		t.Errorf("jwtTokens must survive into the restorable copy:\n%s", restore)
	}
	// And the normalised copy must still drop them, or every minted token is a diff.
	normalised, err := os.ReadFile(filepath.Join(snap, "sos.json"))
	if err != nil {
		t.Fatalf("reading normalised copy: %v", err)
	}
	if strings.Contains(string(normalised), "jwtTokens") {
		t.Errorf("the diff copy must not carry jwtTokens:\n%s", normalised)
	}
}

// `run` is the documented entry point -- "START HERE". A hard stop that fires
// only in `inventory` is not a hard stop, it is a suggestion, and both of these
// leave you migrating onto a cluster where adoption cannot work.
func TestRunEnforcesTheSameHardStopsAsInventory(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(c *cluster)
	}{{
		// The chart would render a SECOND set of AppProjects beside the live
		// ones: nothing is adopted, the snapshot is empty, and the diff that is
		// meant to be the go/no-go gate compares nothing against nothing.
		name: "argocd.namespace mismatch",
		want: "mismatch",
		setup: func(c *cluster) {
			c.write("ic.json", strings.Replace(defaultIntegrationConfig,
				`"namespace": "stakater-tenant-operator",`, `"namespace": "somewhere-else",`, 1))
		},
	}, {
		name: "AppProjects managed from Git",
		want: "GitOps-managed",
		setup: func(c *cluster) {
			c.write("appprojects.json", strings.Replace(defaultAppProjects,
				`{"metadata": {"name": "pe", "namespace": "stakater-tenant-operator"},`,
				`{"metadata": {"name": "pe", "namespace": "stakater-tenant-operator",
     "labels": {"app.kubernetes.io/instance": "appprojects-from-git"}},`, 1))
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cmd := range []string{"run", "inventory"} {
				c := newCluster(t)
				tc.setup(c)
				out, _, err := c.run("--state-dir", c.path("state"), cmd)
				if err == nil {
					t.Errorf("%s continued past %s", cmd, tc.name)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("%s did not name the problem (%q):\n%s", cmd, tc.want, out)
				}
			}
		})
	}
}

// scaffolding.mode Always opts every tenant in at once, so a tenant-by-tenant
// migration is impossible. `run` is the command people actually use, so the
// warning has to reach it and not just `cutover` and `inventory`.
func TestRunWarnsWhenThereIsNoPerTenantOptIn(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplate(t, "scaffolding.mode=Always"))

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	if !strings.Contains(out, "scaffolding.mode is Always") {
		t.Errorf("run did not warn about a mode with no per-tenant opt-in:\n%s", out)
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

// Under scaffolding.mode=OnLabel the opt-in is a LABEL, and both keys default to
// the same string -- so writing the annotation instead would look right in every
// log and scaffold nothing. `run`'s read side is covered above; this is the write
// side.
func TestCutoverWritesTheLabelUnderOnLabel(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplate(t, "scaffolding.mode=OnLabel"))

	_, calls := c.mustRun("cutover", "pe", "--apply")

	del := indexOf(calls, "delete", "extensions.tenantoperator.stakater.com")
	label := indexOf(calls, "label", "tenants.tenantoperator.stakater.com", "enabled=true")
	if label < 0 {
		t.Fatalf("the tenant was never opted in via a label; calls: %v", calls)
	}
	if i := indexOf(calls, "annotate", "enabled=true"); i >= 0 {
		t.Errorf("cutover wrote the opt-in as an annotation under OnLabel: kubectl %s", calls[i])
	}
	if label < del {
		t.Errorf("opt-in label at %d precedes the CR delete at %d -- that is the dual-writer collision", label, del)
	}
}

// A CRD read that FAILS is not a CRD that is absent. Treating it as absent makes
// every Extensions CR invisible: cutover opts the tenant in without draining its
// CR -- the dual-writer collision the tool exists to prevent -- and `run` can
// report a cluster full of Extensions CRs as drained.
func TestACRDReadFailureIsNotTreatedAsDrained(t *testing.T) {
	c := newCluster(t)
	c.write("crd-read-fails", "")

	out, calls, err := c.run("cutover", "pe", "--apply")
	if err == nil {
		t.Fatal("cutover proceeded without being able to read the Extensions CRD")
	}
	for _, call := range calls {
		if mutating(call) {
			t.Errorf("cutover wrote to the cluster while blind: kubectl %s", call)
		}
	}
	if !strings.Contains(out, "could not read CRD") {
		t.Errorf("the error should name the read failure, got: %s", out)
	}
}

// The snapshot is the migration's only rollback record. A failed LIST that
// produces an exit-0 snapshot of zero objects corrupts that record silently:
// every later diff compares nothing, and every restore instruction points at a
// file that does not exist.
func TestSnapshotRefusesWhenTheListCannotBeRead(t *testing.T) {
	c := newCluster(t)
	c.write("appprojects-list-fails", "")

	out, _, err := c.run("snapshot", c.path("snap"))
	if err == nil {
		t.Fatal("snapshot exited 0 without being able to list AppProjects")
	}
	if !strings.Contains(out, "refusing") {
		t.Errorf("the error should refuse the write, got: %s", out)
	}
	if _, statErr := os.Stat(c.path("snap")); !os.IsNotExist(statErr) {
		t.Errorf("a snapshot directory was written anyway")
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
	// MISSING, specifically -- not just "something is wrong". An unseeded chart
	// and a wrongly-seeded one need the same action but read differently, and the
	// per-row marker is what tells a reader which one they have.
	if !strings.Contains(out, "MISSING") || !strings.Contains(out, "do not match the IntegrationConfig") {
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

	// Keyed on the section header, not a sentence. Prose gets reworded; the
	// header is the structural claim -- "there is no Extensions CR here" -- and
	// the run must still reach the opt-in below it.
	out, calls := c.mustRun("cutover", "legacy", "--apply")
	if !strings.Contains(out, "EXTENSIONS CR  --  none") {
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
	// The instruction must point at the applyable restore copy -- the normalised
	// file has no apiVersion or kind, so kubectl apply rejects it mid-incident.
	if !strings.Contains(out, "restore/sos.json") {
		t.Errorf("the restore instruction does not point at the restore copy:\n%s", out)
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

// An AppProject with no Extensions CR is adopted, not drained. Without that
// branch, `run` prints "delete the manifest defining Extensions/." under "commit
// this" -- an instruction to delete a manifest that does not exist, name blank.
func TestRunDoesNotAskYouToDeleteAManifestThatDoesNotExist(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	c.write("appprojects.json", appProjectsNoLegacyOwner)
	c.write("extensions.json", `{"items": []}`)
	c.mustRun("--state-dir", c.path("state"), "run", "--apply") // snapshot

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	if strings.Contains(out, "Extensions/.") || strings.Contains(out, "Extensions/\n") {
		t.Errorf("named an Extensions CR that does not exist:\n%s", out)
	}
	if !strings.Contains(out, "nothing to drain") {
		t.Errorf("an orphan AppProject must be described as an adoption:\n%s", out)
	}
}

// A leftover Extensions CR whose Tenant exists AND is opted in is not cleanup
// work: both writers hold that AppProject right now, which is the collision the
// whole tool is built to avoid. Listing it as "names a tenant that does not
// exist" is both wrong and reassuring.
func TestRunNamesADualWriterCollisionRatherThanFilingItAsCleanup(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	// sos is opted in in defaultTenants, has an AppProject, and still has a CR.
	c.write("appprojects.json", `{"items": [
  {"metadata": {"name": "sos", "namespace": "stakater-tenant-operator"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}}
]}`)
	c.write("extensions.json", `{"items": [
  {"metadata": {"name": "sos-extensions"}, "spec": {"tenantName": "sos", "argoCD": {}}}
]}`)
	c.mustRun("--state-dir", c.path("state"), "run", "--apply") // snapshot

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	if strings.Contains(out, "name tenants that do not exist") {
		t.Errorf("tenant sos exists and is opted in, so this is a collision:\n%s", out)
	}
	if !strings.Contains(out, "ALREADY opted in") || !strings.Contains(out, "sos-extensions") {
		t.Errorf("the collision was not named:\n%s", out)
	}
}

// --batch reaches jq as --argjson, so an unvalidated value dies there with
// `invalid JSON text passed to --argjson`; and --batch 0 silently offers no
// tenant, which reads as a finished migration.
func TestBatchMustBeAPositiveInteger(t *testing.T) {
	for _, bad := range []string{"notanumber", "0", "-1", "1.5"} {
		t.Run(bad, func(t *testing.T) {
			c := newCluster(t)
			out, _, err := c.run("--batch", bad, "run")
			if err == nil {
				t.Fatalf("--batch %s was accepted:\n%s", bad, out)
			}
			if !strings.Contains(out, "positive integer") {
				t.Errorf("--batch %s did not explain itself:\n%s", bad, out)
			}
		})
	}
}

// The token warning costs a credential when it is missed, and `run` is the
// documented entry point and the only command a GitOps fleet ever uses -- so it
// cannot be the one command that stays silent about tokens.
func TestRunWarnsAboutRoleTokensBeforeTheCommitBlock(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	c.write("extensions.json", extensionsRetained)
	c.write("tenants.json", tenantsWithPurgeIntentCarried)
	c.write("appprojects.json", appProjectsWithTokens)
	c.mustRun("--state-dir", c.path("state"), "run", "--apply") // snapshot

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	warnAt := strings.Index(out, "ArgoCD role tokens stop working")
	commitAt := strings.Index(out, "----- commit this")
	if warnAt < 0 {
		t.Fatalf("run said nothing about the token it is about to destroy:\n%s", out)
	}
	if !strings.Contains(out, "ci-deploy") {
		t.Errorf("the token id was not named:\n%s", out)
	}
	if commitAt >= 0 && commitAt < warnAt {
		t.Errorf("the warning is inside/after the commit block (warn %d, commit %d):\n%s",
			warnAt, commitAt, out)
	}
}

// "The object still exists" is not the claim the runbook makes -- it promises the
// same UID and no gap. A delete-and-recreate keeps the name while losing the
// tokens and dangling every ownerRef on the old UID, and a name check calls that
// a success.
func TestCutoverRefusesASameNamedReplacement(t *testing.T) {
	c := newCluster(t)
	c.write("appproject-uid-changes", "")

	out, _, err := c.run("handover", "pe", "--apply", "--yes")
	if err == nil {
		t.Fatalf("a recreated AppProject was reported as retained:\n%s", out)
	}
	if !strings.Contains(out, "REPLACED, not retained") || !strings.Contains(out, "uid-pe") {
		t.Errorf("the UID change was not the reported cause:\n%s", out)
	}
}

// A tenant whose Extensions CR never produced an AppProject -- `inventory` calls
// it out by name. Reading that absence as data loss points the operator at a
// restore file no snapshot can hold.
func TestCutoverDoesNotClaimDataLossWhenThereNeverWasAnAppProject(t *testing.T) {
	c := newCluster(t)
	// sos has an Extensions CR with purge unset; give it no AppProject.
	c.write("appprojects.json", `{"items": [
  {"metadata": {"name": "pe", "namespace": "stakater-tenant-operator"},
   "spec": {"description": "Managed by Extensions controller", "destinations": [], "roles": []}}
]}`)

	out, _ := c.mustRun("handover", "sos", "--apply", "--yes")
	if strings.Contains(out, "disappeared") {
		t.Errorf("reported data loss for an AppProject that never existed:\n%s", out)
	}
	if !strings.Contains(out, "no AppProject existed before") {
		t.Errorf("did not say why there was nothing to verify:\n%s", out)
	}
}

// mkdir happens before the first object is written, so a snapshot that dies
// partway leaves the directory behind -- and a directory is not a rollback
// record. Cutting tenants over against an empty one is unrecoverable.
func TestAFailedSnapshotDoesNotCountAsTaken(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
	c.write("extensions.json", extensionsRetained)
	c.write("tenants.json", tenantsWithPurgeIntentCarried)
	// No AppProjects, but Extensions CRs remain: snapshot refuses to write a
	// record of nothing, after having already created the directory.
	c.write("appprojects.json", `{"items": []}`)

	if _, _, err := c.run("--state-dir", c.path("state"), "run", "--apply"); err == nil {
		t.Fatal("snapshot of an empty namespace with CRs left should have failed")
	}
	if _, err := os.Stat(c.path("state/snapshot")); err != nil {
		t.Skipf("the directory was not left behind, so there is nothing to mistake: %v", err)
	}

	out, _ := c.mustRun("--state-dir", c.path("state"), "run")
	if !strings.Contains(out, "snapshot every AppProject") {
		t.Errorf("a failed snapshot was ticked off as done:\n%s", out)
	}
}

// YAML escapes an embedded single quote by doubling it. Both emitters have to do
// it: the one `run` uses hands you a manifest you are told to commit.
func TestOverrideValuesCarryingAQuoteStayValidYAML(t *testing.T) {
	const want = `'["https://github.com/acme/o''brien-gitops"]'`
	for _, args := range [][]string{{"annotations"}, {"run"}} {
		t.Run(args[0], func(t *testing.T) {
			c := newCluster(t)
			c.write("parent-template.json", renderParentTemplateWith(t, seededLists))
			c.write("extensions.json", extensionsWithQuotedOverride)
			c.write("tenants.json", tenantsFromGit)
			out, _ := c.mustRun(append([]string{"--state-dir", c.path("state")}, args...)...)
			if !strings.Contains(out, want) {
				t.Errorf("%v did not double the quote (want %s):\n%s", args, want, out)
			}
		})
	}
}

// Every command this tool tells you to run has to carry the flags it was given
// itself. A hint that drops --context or --state-dir sends the next invocation at
// the wrong cluster, or at a snapshot directory it will report as absent.
func TestPasteableCommandsCarryTheGlobalFlags(t *testing.T) {
	c := newCluster(t)
	c.write("parent-template.json", renderParentTemplateWith(t, seededLists))

	out, _ := c.mustRun("--context", "prod", "--state-dir", c.path("state"), "run")
	for _, want := range []string{"--context=prod", "--state-dir=" + c.path("state")} {
		if !strings.Contains(out, want) {
			t.Errorf("no command in the output carries %s:\n%s", want, out)
		}
	}
	if regexp.MustCompile(`mto-argocd-migrate annotations --apply`).MatchString(out) {
		t.Errorf("a hint dropped the global flags:\n%s", out)
	}
}
