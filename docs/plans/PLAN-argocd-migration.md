# Migrating a tenant from `ExtensionsArgoCDReconciler` to the chart

> **Status:** Draft — under review
> **Author / driver:** Ayesha Arshad
> **Last updated:** 2026-08-23
> **Type:** Operational (tooling + runbook). No chart behaviour changes.
> **Related:** [TASKS §E](TASKS-argocd-extension.md#e--migration) · [live-deployment.md](../live-deployment.md) · [upstream-issues.md](../upstream-issues.md) (TOV2-1, TOV2-5, TOV2-9) · [rollback.md](../rollback.md) · branch `feat/argocd-migration`, stacked on `feat/chart-scaffold-and-render-tests`

## TL;DR

The recorded design is a five-subcommand CLI — `inventory`, `strip-owner-refs`, `snapshot`,
`cutover`, `diff`. This plan collapses it to **one subcommand, `cutover`, with `--dry-run`
(default) and `--apply`**, plus a separate `rollback`. The collapse is possible because the
chart's rendering is reproducible in-process: `tests/render/engine.go` already mirrors
template-operator-v2's renderer exactly, so the tool can **predict** the post-migration
AppProject from live cluster inputs and diff it against the live object *before touching
anything*. That turns `inventory`, `snapshot` and `diff` from separate human steps into
side-effects of a dry-run, and makes `strip-owner-refs` an ordered step inside apply.

The operator types three commands per cluster, not five per tenant: a dry run, their own
`helm upgrade` with a values file the dry run writes for them, and an apply.

## Verified on a cluster, 2026-08-23

Before this plan was accepted, its load-bearing claims were run against
`kind-mto-argocd-e2e` — real ArgoCD 3.4.6, real template-operator-v2 **v0.0.6**, MTO's CRDs
without MTO, this chart installed from the branch. The before-state was `AppProject/pe` built
by hand to match `ExtensionsArgoCDReconciler`'s output exactly, including the fossil `v1beta2`
`Tenant` ownerRef, the `multi-tenant-operator/predicate` label, and a `jwtTokens` entry.

| Claim | Result |
|---|---|
| Installing the chart adopts nothing | **Held.** Only the parent TemplateInstance appeared; `AppProject/pe` kept its UID, description and content untouched |
| The generated group hash formula matches the controller's | **Held.** `sha256("pe-owner")[:6]` = `489fab`, which is the group name on the real live cluster |
| Adoption preserves `metadata.uid` | **Held.** `e7c15900-…` before and after |
| The equivalence diff is only the two documented deltas | **Held.** `spec.description` changed; `roles[0].groups` gained `ayesha.arshad@stakater.com`; `destinations`, both resource lists, `sourceRepos`, and all three roles' `name`/`description`/`policies` byte-identical. `roles[1]`/`roles[2]` groups correctly gained nothing, the tenant having no editors or viewers |
| Adoption preserves `spec.roles[].jwtTokens` | **FAILED — see below.** The token was destroyed. The old controller destroys it too (`pkg/builders/appproject.go:143`), so this is a reproduced defect, not a regression |
| The fossil ownerRef defeats tov2's retain policy | **Held, and it is worse than "latent".** Two probe AppProjects both carrying `deletion-policy: Orphan`, one with a `v1beta2` Tenant ownerRef and one without; deleting the Tenant garbage-collected the one with the ref and left the other standing |
| `clusterResourceWhitelist` empty-vs-absent is a real diff class | **Refuted.** argo-cd v3.4.6 `types.go:2785` carries `json:"clusterResourceWhitelist,omitempty"`, so `make([]T, 0)` serialises to nothing. No normalisation needed |
| A returning `Extensions` CR cannot reclaim a migrated AppProject (the safety net, not the plan — the CR is removed from Git in the same change) | **Held, both paths.** Simulating the controller's `CreateOrPatch` write was denied by `sync-enforcement.templates.v2.stakater.com`; so was its purge-path `delete`. `description`, `uid` and `resourceVersion` all unmoved |
| Reverting the Tenant opt-in annotation is safe | **Refuted, and this is the sharpest hazard in the design.** On v0.0.6 the AppProject was **deleted in 2 seconds**; re-adding the annotation recreated it with a **new UID** (`03fe60a9` → `d30ea975`) |

### What is NOT yet tested, and why it matters

The table above is the whole of the evidence. Everything else in this plan is reasoned from code
that was read, and should be treated as such until the suites in the Test plan exist.

| Untested | Why it could not be tested here | Where it gets closed |
|---|---|---|
| **The `onDeletePurgeAppProject: true` branch** — that deleting an armed CR really does destroy the AppProject, and that disarming really does prevent it | Needs the real `ExtensionsArgoCDReconciler`. MTO is a licensed private image and is deliberately not installed on the kind cluster | The envtest stub controller (Test plan). This is the **highest-value untested item** — it is the only path that destroys data |
| **"Delete the CR → the AppProject survives"** against the real controller | Same reason. It *was* measured on the live cluster on 2026-08-07 and stands as evidence there | The live `pe` cutover, and the envtest stub |
| **The liveness / observation gate** — that a label-only change triggers a reconcile and advances `status…lastTransitionTime` | Same reason; there is no controller here to observe | Envtest stub. Until then, R5's mechanism is a code reading of the event filter, not a measurement |
| **Anything ArgoCD-managed** — the Lane B/C/D flows, prune-as-deleter, annotation drift-revert | Needs a real `Application` and a Git source reachable from the cluster | E2E, or the first customer cluster under supervision. Note the *consequence* of annotation drift **was** measured (delete in 2s, new UID) — what is untested is ArgoCD being the thing that causes it |
| **tov2 v0.0.7 retain behaviour** | The kind cluster runs v0.0.6, where retain is not enforced. That is why de-annotating deleted the AppProject rather than releasing it | `kind-tov2-retain`, or pinning `TOV2_CHART_VERSION=0.0.7` |
| **Multi-tenant `--all`** | The kind cluster had one migratable tenant in these runs; the live cluster is capped at two Tenants by MTO's free tier | E2E with three fixture tenants |

### The failure: `roles[].jwtTokens` do not survive, and not only during migration

`values.yaml` currently claims, of the `spec.roles[*].jwtTokens` exemption:

> Exempting it is safe: this chart never renders jwtTokens, and server-side apply only owns
> the fields it sets, so tokens survive every re-render regardless.

The second half is false. Measured, in this order:

1. A token was minted onto the chart-managed `AppProject/pe` by patching
   `spec.roles[0].jwtTokens`. **The strict webhook allowed it** — so the `ignoreFields`
   exemption does do one of its two jobs.
2. The object was then left **idle for 60 seconds**. The token survived and
   `metadata.resourceVersion` never moved, so tov2's drift detection is also correctly
   exempting it. `ignoreFields` works.
3. A single unrelated re-render was triggered — removing the `stakater.com/tenant` label from
   one namespace, which changes `destinations` and nothing else. `destinations` shrank from 4
   to 3 as expected, and **`jwtTokens` became `null`**.

The mechanism is not drift and not a tov2 bug. `spec.roles` on the AppProject CRD declares no
`x-kubernetes-list-type`, so server-side apply treats the list as **atomic**: the manager that
applies it owns the whole list, sub-fields included. `managedFields` shows
`template-operator [Apply]` holding `f:spec.f:roles` with no sub-selectors. So every apply
replaces all three roles wholesale with the chart's rendered version, which never contains
`jwtTokens`. `ignoreFields` cannot help — it governs drift comparison and webhook admission,
not the shape of the apply.

**This is a pre-existing chart defect, not a migration defect.** Any tenant that mints an
ArgoCD project role token loses it on the next namespace add or remove, IntegrationConfig
change, Tenant annotation change, or chart upgrade. Migration merely makes it certain, because
adoption is itself an apply.

It changes this plan in three places, all recorded below: requirement **R2** is split, the
failure catalogue gains a row, and the tool gains a hard refusal. It also needs a decision
that is **out of this PR's scope** — see the first Open Question.

Reproduction is in the scratchpad `REPRO.md` and is three commands; it should become a test
either way.

### How the chart takes the AppProject over, and why the old controller cannot take it back

Worth stating precisely, because "reclaim" suggests a fight and there is none. Adoption is a
server-side apply onto the **same object** — same name, same namespace, so same object. The
API server transfers *field* ownership; nothing is deleted and recreated. Measured
`managedFields` on `spec` immediately after adoption:

```text
template-operator        [Apply]:  f:clusterResourceWhitelist, f:description,
                                   f:destinations, f:namespaceResourceBlacklist, f:roles
kubectl-client-side-apply [Update]: f:clusterResourceWhitelist, f:destinations,
                                   f:namespaceResourceBlacklist
```

The chart's manager took `f:description` and `f:roles` outright, because their values differed
and the apply carries `ForceOwnership`. The three fields whose values were already identical
stayed **co-owned** — which is also the mechanism behind the surviving
`multi-tenant-operator/predicate` label: server-side apply never removes a field it does not
set.

And once tov2 owns it, the old controller is locked out rather than racing. Both of its write
paths were tried against the migrated object and both were refused:

```text
# its CreateOrPatch write
Error from server (Forbidden): admission webhook "sync-enforcement.templates.v2.stakater.com"
  denied the request: resource is managed by sync-enabled TemplateInstance
  multi-tenant-operator/mto-argocd-tenant-pe; modifications to enforced fields are not allowed

# its purge-path delete
Error from server (Forbidden): ... deletion is not allowed
```

`description`, `uid` and `resourceVersion` were all unchanged afterwards. This reproduces the
2026-08-06 live measurement recorded in upstream-issues.md ("Dual-run collision — measured, not
a bug") on a second cluster and a newer operator build.

So a returning `Extensions` CR **cannot** corrupt the AppProject. What it does instead is
worse in one specific way: the controller enters a permanent error-retry loop, and the failure
surfaces only on the `Extensions` CR's own status, where nobody is looking. The AppProject stays
correct while the cluster carries a broken controller indefinitely. That is why the CR has to
come out of Git as part of the migration commit — not to protect the AppProject, but because
nothing will tell you it did not.

## Context

Today `AppProject/<tenant>` is owned by `ExtensionsArgoCDReconciler`, driven by a
cluster-scoped `Extensions` CR. Read from `stakater-ab/tenant-operator@main`:

- `api/v1alpha1/extensions_types.go` — `ExtensionsSpec{TenantName string; ArgoCDConfig *ArgoCDConfig}`,
  `ArgoCDConfig{AppProject *AppProjectConfig; OnDeletePurgeAppProject bool}`,
  `AppProjectConfig{SourceRepos []string; NamespaceResourceBlacklist []metav1.GroupKind; ClusterResourceWhitelist []metav1.GroupKind}`.
  Cluster-scoped (`+kubebuilder:resource:path=extensions,scope=Cluster`).
- `internal/controller/extensions/argocd/extensions_argocd_controller.go` — the reconciler.
  Finalizer `tenantoperator.stakater.com/appproject` on the **Extensions CR** (not on the
  AppProject). Watches the IntegrationConfig and Namespaces; event filter is
  `predicate.Or(GenerationChangedPredicate{}, LabelChangedPredicate{})`.
- `internal/controller/extensions/argocd/extensions_utils.go` — `addRoles`, and
  `removeOldOwnerReference`, which strips `tenantoperator.stakater.com/v1beta2` `Tenant`
  ownerRefs from the AppProject on every sync that finds the object already present.
- `config/manager/extensions-argocd-deploy.yaml` — a **separate** Deployment,
  `extensions-argocd-controller`, label `control-plane: controller-manager-extensions-argocd`,
  env `EXTENSIONS_ARGOCD_CONTROLLER=true`.

The chart replaces it: `charts/mto-extension-argocd` renders one child TemplateInstance per
opted-in Tenant (`files/parent-gotemplate.tpl`), each rendering one AppProject
(`files/child-gotemplate.tpl`). Opt-in is `argocd.tenantoperator.stakater.com/enabled=true`
on the Tenant, because `scaffolding.mode` defaults to `OnAnnotation`.

Nothing in this repository migrates an existing tenant. `templates/NOTES.txt` currently tells
the operator that tooling "ships in a follow-up release" — this is that release.

### What the two implementations actually do differently

This is the substance of the migration, so it is stated field by field. Every "controller"
column below is from code read in this session; every "chart" column is from the gotemplates
in this repository.

| AppProject field | Controller | Chart | Migration consequence |
|---|---|---|---|
| `spec.description` | `"Managed by Extensions controller"` (`extensions_argocd_controller.go`, `handleArgoCDConfiguration`) | `"Managed by mto-extension-argocd"` | **Expected diff.** Already asserted in `tests/render/equivalence_test.go`. |
| `spec.sourceRepos` | `extensions.Spec.ArgoCDConfig.AppProject.SourceRepos`, **no IntegrationConfig fallback** | Tenant annotation `…/source-repos`, else `argocd.sourceRepos.pattern`, else `[]` | Carry the CR's list into the annotation. Chart `pattern` must stay empty or every tenant gains a repo it did not have. |
| `spec.destinations` | Namespaces labelled `stakater.com/tenant=<t>`, minus privileged, **then the ArgoCD namespace appended unconditionally** (`populateArgoCDSettings`) | Same filter, plus `if not (has $argoNs $nsList)` | Equal on every cluster where the ArgoCD namespace is privileged or unlabelled — including the live cluster. Differs (controller emits a duplicate, chart does not) where the ArgoCD namespace carries the tenant label and is not privileged. Classified **benign**. |
| `spec.clusterResourceWhitelist` | IntegrationConfig `.spec.integrations.argocd.clusterResourceWhitelist`, replaced wholesale when `extensions.…ClusterResourceWhitelist != nil` | Chart value `argocd.clusterResourceWhitelist`, replaced wholesale by the `…/cluster-resource-whitelist` annotation when present | Two-part carry: IC list → chart values (fleet-wide, one `helm upgrade`), per-CR list → per-Tenant annotation. `nil` vs `[]` maps exactly onto the chart's absent-vs-`[]` contract. |
| `spec.namespaceResourceBlacklist` | Same shape, same override rule | Same | Same. |
| `spec.roles[].name`, `.description`, `.policies` | `addRoles`, `extensions_utils.go` | `child-gotemplate.tpl` | Byte-identical, pinned by `tests/render/equivalence_test.go`. |
| `spec.roles[].groups` | `[generated, privilegedGroups…, tenant groups…]` | same **+ the tenant's users** | **Expected diff.** README documents it; the equivalence test derives it from the fixture. |
| `spec.roles[].jwtTokens` | Not written by either side | Exempted from `sync.mode: strict` via `sync.ignoreFields`, never rendered | Must survive. Survives only if adoption preserves the object; see UID below. |
| `metadata.ownerReferences` | Never added by this controller; `removeOldOwnerReference` strips a fossil `v1beta2` `Tenant` ref left by an older MTO | Not managed | **The tool strips it.** See "why the ownerRef matters" below. |
| `metadata.labels` | `multi-tenant-operator/predicate: ""` | Not managed; server-side apply does not remove another manager's field | Survives adoption. Benign — no controller in `tenant-operator@main` reads AppProjects other than the extensions one (verified: `git grep -l AppProject` over non-test Go returns only the extensions package, the builders/objects helpers, and type definitions). |
| `metadata.annotations` | none | `templates.v2.stakater.com/deletion-policy: Orphan` when purge is off | Chart-added; expected. |
| `metadata.finalizers` | none on the AppProject | `templates.v2.stakater.com/template-instance` added by tov2 | Chart-added; expected. |
| `metadata.uid` | — | — | **Must not change.** Adoption is server-side apply onto the existing object; verified live 2026-08-07, UID `e5401603-…` held across the whole procedure. |

Two behaviours in the controller are worth naming because they are the reason the migration
is safe at all:

**`handleDeletion` retains by default.** With `Spec.ArgoCDConfig == nil` it logs
"ArgoCDConfig is not present … No action required" and removes the finalizer. With
`ArgoCDConfig != nil` it deletes the AppProject **only** if
`OnDeletePurgeAppProject` is true. So "delete the CR, keep the AppProject" is the
controller's own default path — not a trick, and measured live on 2026-08-07.

**The finalizer is released by nothing but this controller.** Which is why the recorded
reversal in §E stands: a cluster-wide "controller off first" wedges every CR in
`Terminating`.

### Why the fossil ownerRef matters more after adoption than before

Before adoption, a `v1beta2` `Tenant` ownerRef on the AppProject is inert-ish: the controller
strips it whenever it next syncs that object. After adoption the chart owns the object and
`sync.mode: strict` forbids anyone else from editing it — so the ref becomes permanent. And
an ownerRef is enforced by the garbage collector, which knows nothing about
`templates.v2.stakater.com/deletion-policy: Orphan`. A retained AppProject with a live
ownerRef to a Tenant is deleted the moment that Tenant is deleted, silently defeating the
retain guarantee the chart advertises. So the strip must happen, and it must happen
**before** adoption, while the object is still plainly editable.

## Goals and non-goals

**Goals**

- One command per tenant, and one command for the whole cluster: `cutover <tenant>` and `cutover --all`.
- The equivalence diff is shown **before** any mutation, computed from live inputs, not promised afterwards.
- Never two writers on one AppProject, enforced in code, not by runbook discipline.
- Adoption preserves `metadata.uid`, asserted by the tool and failed loudly if it does not.
- Seed values (`argocd.clusterResourceWhitelist`, `argocd.namespaceResourceBlacklist`, `argocd.namespace`) and per-tenant overrides are **derived** from the live IntegrationConfig and the live Extensions CRs — the human never hand-copies a resource list.
- Interrupted at any point, re-running the same command converges. No separate resume mode.
- A documented, tested rollback, honest about what it costs.

**Non-goals**

- No Helm hook, no Job, no image. The chart stays image-free (§E reversal E1).
- No change to chart rendering behaviour. This PR adds a tool, docs, and tests; the only file it touches under `charts/` is the migration paragraph in `NOTES.txt`.
- Not the `sourceRepos` rollout. A tenant with no `sourceRepos` today gets a faithfully useless AppProject; fixing that is a separate, later step, as recorded in live-deployment.md.
- Not Phase 3. Removing the controller, the CRD and the API surface is TASKS §F; this plan produces the release-note text for it and nothing more.
- Not a general-purpose `Extensions` migrator. ArgoCD is the only extension the CR carries.

## Requirements

1. **R1 — No overlap.** For any tenant, at no point may both `Extensions/<x>` (with
   `spec.tenantName == t`) exist and `Tenant/t` carry the opt-in annotation. Enforced by
   ordering *and* by a pre-write assertion immediately before the annotation patch.
2. **R2a — Identity preserved.** `metadata.uid` of the AppProject is unchanged from before
   the run to after it. Verified, not assumed. **Measured 2026-08-23: holds.**
3. **R2b — Token loss is refused, not survived.** `spec.roles[].jwtTokens` **cannot** be
   preserved through adoption: server-side apply of the atomic `spec.roles` list destroys them
   (see "Verified on a cluster"). So the requirement inverts — the tool must **refuse to
   migrate any tenant whose AppProject carries a `jwtTokens` entry**, name the roles and token
   ids, and say plainly that migrating will destroy them. `--accept-token-loss` is the only way
   past it, and dry-run reports the count for every tenant whether or not it is zero.
4. **R3 — Seeded or refused.** If the chart's effective `argocd.clusterResourceWhitelist` /
   `namespaceResourceBlacklist` / `namespace` do not match the IntegrationConfig's
   `integrations.argocd.*`, the tool refuses to apply and prints the values file to fix it.
5. **R4 — Purge disarmed before deletion.** If any tenant's Extensions CR has
   `spec.argoCD.onDeletePurgeAppProject: true`, the tool patches it to `false` and confirms
   the controller has observed the patch before deleting the CR. **Under GitOps this is the
   requirement that carries the most weight**, because ArgoCD's prune — not this tool — may
   issue the delete, and the operator cannot choose when. See GitOps considerations.
6. **R5 — Liveness proven.** The tool proves the old controller is reconciling before it
   deletes a CR whose finalizer only that controller releases.
7. **R6 — Fail-closed on the fleet-killers.** The tool refuses to run at all when the
   IntegrationConfig's `accessControl.privileged.namespaces` is absent, null, contains an
   empty pattern, or contains a pattern that does not compile; and when tov2's
   sync-enforcement webhook lacks its `matchConditions` filter.
8. **R7 — Idempotent and convergent.** Re-running after any failure is safe and makes
   progress. State lives on the cluster, not in a local journal.
9. **R8 — Snapshot before mutation.** A restorable dump of the AppProject and the Extensions
   CR is on disk before the first write, and the run aborts if it cannot be written.
10. **R9 — Read-only by default.** `--dry-run` is the default; `--apply` is explicit.
11. **R10 — Explicit context.** No use of the ambient `current-context`, matching the policy
    in live-deployment.md and `tests/e2e`.
12. **R11 (inferred)** — The tool must run from this repository with no credentials beyond
    the operator's own kubeconfig, and add no new direct Go dependency. `go.mod` already has
    `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`, `sprig/v3` and `sigs.k8s.io/yaml`.

## Assumptions

- The cluster runs one `Extensions`-consuming controller (`extensions-argocd-controller`) and
  one release of this chart. Multiple chart releases are already refused by the chart's fixed
  Template/Role names, per `values.yaml`.
- Migration is performed by whoever installs the chart — a Stakater/SAAP operator with a
  checkout of this repository and Go 1.26 available. See Open Questions for the
  no-Go-toolchain case.
- The operator can run `helm upgrade` (or push to the GitOps repo that does), because the
  fleet-wide seed values cannot be written by this tool without fighting GitOps.
- Tenants are on the order of tens per cluster, not thousands. The live cluster is capped at
  two by MTO's free tier.
- `roles[].jwtTokens` may exist in production and are unrecoverable if lost, so R2 is a hard
  requirement rather than tidiness.
- Whoever runs `--apply` can watch it. There is no unattended mode and none is wanted.

## Unknowns

Each of these becomes a Pre-work item. None of them changes the shape of the design; three of
them change what the diff classifier must tolerate.

- ~~**Does argo-cd v3.4's `AppProjectSpec` carry `omitempty` on `clusterResourceWhitelist`?**~~
  **Closed 2026-08-23: yes.** `argo-cd/v3@v3.4.6:pkg/apis/application/v1alpha1/types.go:2785`
  reads `json:"clusterResourceWhitelist,omitempty"`, so the non-nil empty slice
  `builders.AppProject().ClusterResourceWhitelist()` produces from a nil input serialises to
  nothing, exactly as the chart's omission does. There is no empty-vs-absent diff class for
  this field and the classifier needs no normalisation for it.
- **Does `AppProject/pe` on the live cluster currently carry a `v1beta2` `Tenant` ownerRef?**
  The 2026-08-07 run found and stripped one; the same-day rollback then deleted and
  **recreated** the object, and `handleArgoCDConfiguration` never adds an ownerRef — so it is
  probably clean now and the strip step will be a no-op for `pe`. Matters because the live
  dry-run is the acceptance gate and I want to know in advance whether it exercises the strip
  path at all. *Verify by:* `kubectl get appproject pe -n stakater-tenant-operator -o jsonpath='{.metadata.ownerReferences}'`.
- **Does `Extensions.status.argocd.conditions[].lastTransitionTime` reliably advance on every
  successful reconcile on the live build?** `updateArgoCDConfigStatus` sets
  `LastTransitionTime: metav1.Now()` and replaces the whole conditions slice on every call, so
  it should. Matters because R5's liveness probe is built on it. *Verify by:* labelling
  `Extensions/pe-extensions` with a throwaway label (a `LabelChangedPredicate` trigger) and
  watching the timestamp — a read-mostly operation, one label, reversible.
- **Does patching a Tenant at `v1beta3` on the live cluster route through MTO's conversion
  webhook?** `tests/crds/README.md` states that `Tenant`'s storage version is `v1beta3` and
  `IntegrationConfig`'s is `v1beta1`, so a request at those versions needs no conversion — and
  the chart already reads both there against the live cluster successfully. That is enough to
  settle the design (**pin `v1beta3` explicitly**, never a discovered preferred version, which
  could move to a version whose conversion webhook is broken per TASKS A3(a)). What is not
  settled is whether a *write* at the storage version is equally exempt; a read that works does
  not prove a patch does. Matters because every apply ends in a Tenant patch. *Verify by:* the
  live dry-run, which now includes a `--dry-run=server` Tenant patch as a preflight probe (see
  "Preflight" below), so the acceptance gate answers this before any real write.
- **Does the live cluster's tov2 v0.0.7 rollback path preserve the UID now that retain is
  enforced?** The 2026-08-07 rollback was measured on v0.0.5, where removing the annotation
  *deleted* the AppProject — hence the ~8s gap and the changed UID. On v0.0.7 the same step
  should *release* it, letting the old controller adopt the surviving object in place with its
  UID intact. Matters because it would turn rollback from destructive into non-destructive for
  the *object*, which changes what MIGRATION.md must warn about. **Partially answered
  2026-08-23:** on v0.0.6 the kind cluster behaved as documented — de-annotating deleted
  `AppProject/pe` outright, confirming TOV2-1 on that build. The v0.0.7 half is still open, and
  note that `jwtTokens` are lost on the rollback path too regardless of which build, since the
  old controller's `CreateOrPatch` writes `roles` as well. *Verify by:* the `kind-tov2-retain`
  cluster, or by pinning `TOV2_CHART_VERSION=0.0.7` in the e2e-kind lane.
Two Unknowns were opened and closed while drafting, recorded here because the answers are
load-bearing:

- **`Extensions` is single-version and needs no edits to vendor.**
  `tenant-operator@main:config/crd/bases/tenantoperator.stakater.com_extensions.yaml` declares
  exactly one version, `v1alpha1` (served, storage), `scope: Cluster`, and carries neither a
  `spec.conversion` block nor a `cert-manager.io/inject-ca-from` annotation — so unlike the
  Tenant and IntegrationConfig copies, it vendors into `tests/crds/` verbatim with none of the
  two edits `tests/crds/README.md` documents. The envtest plan below depends on this.
- **`Tenant`'s storage version is `v1beta3`; `IntegrationConfig`'s is `v1beta1`.** Stated in
  `tests/crds/README.md`, and it matches the versions the chart's parent TemplateInstance
  requests (`templates/30-parent-templateinstance.yaml`).

Note that the live cluster's token has expired (`kubectl` returns "the server has asked for
the client to provide credentials"), so none of the cluster-side Unknowns could be closed in
this session. They need one `oc login --web`.

## Alternatives considered

The user asked for options, and there are genuinely three credible shapes here plus a
do-nothing. They differ in one thing: **where the equivalence check happens relative to the
mutation.** That single axis decides how many steps a human types and how much of the safety
is enforced rather than documented.

### Alternative A: the recorded five-subcommand CLI

**Approach.** `mto-argocd-migrate inventory` → read the chart config and report unseeded
lists. `strip-owner-refs` → patch the AppProjects. `snapshot` → dump every AppProject to
disk. `cutover <tenant>` → delete the CR, opt in, wait. `diff` → compare live AppProjects
against the snapshot. The human seeds chart values by hand between `inventory` and `cutover`,
and reads `diff` afterwards to find out whether it worked.

**Pros**

- Each command does one thing and is trivially auditable.
- `snapshot`/`diff` are useful independently of a migration.
- It is the design already on record, and one live run against `pe` proved the sequence works.

**Cons**

- The equivalence check happens **after** the mutation. `diff` tells you the migration was
  wrong once the AppProject has already been rewritten and the CR is gone. Recovery is
  rollback, which costs a delete-and-recreate.
- The seeding step is a *documented* prerequisite, not an enforced one. Forget it and the diff
  is non-empty by construction for every tenant at once — the exact failure §E warns about.
- `snapshot` compares against itself: it captures the pre-state and `diff` compares live to
  pre-state. It cannot tell "the chart rendered something different from the controller" apart
  from "someone edited an AppProject in between".
- Five commands × per-tenant ordering is a runbook a human executes under pressure. Every
  ordering constraint in §E is enforced by the human reading the runbook in the right order.

**Lifecycle scenarios.** *Tenant cutover:* five invocations, three of them cluster-wide, two
per-tenant, with a hand-run `helm upgrade` in the middle. *Interrupted mid-`cutover`:* the CR
is gone and the tenant is not annotated; the AppProject is unmanaged and static. Recovery is
to run `cutover` again — but `cutover` as specified has no idea it is resuming, so it tries to
delete a CR that no longer exists. *Operator forgets `strip-owner-refs`:* nothing fails, and
the tenant carries a latent GC bomb. *Two writers:* prevented only by running the commands in
the documented order.

**Complexity.** Five command paths, one shared cluster client, a snapshot file format, a
differ. No prediction engine, so no dependency on the render harness. Roughly the smallest
implementation of the four.

**Verdict:** Acceptable but not preferred. It is safe if executed perfectly and silently
wrong if not, and the wrongness is discovered after the irreversible step.

### Alternative B: one convergent `cutover` with predictive dry-run — **recommended**

**Approach.** One subcommand. `cutover <tenant>` (or `--all`) defaults to `--dry-run`: it
reads the installed chart's own `Template/mto-argocd-parent` and `Template/mto-argocd-child`
off the cluster, runs both gotemplates in-process through the same engine
`tests/render/engine.go` already mirrors, feeds them the live Tenants / Namespaces /
IntegrationConfig **with the target tenant's opt-in and derived override annotations applied
in memory**, and diffs the resulting AppProject against the live one. It prints the plan: the
seed values that are missing, the annotations it will write, the ownerRefs it will strip, the
purge flag it will disarm, and the classified diff. `--apply` executes exactly that plan and
re-verifies against reality afterwards.

`inventory` is what dry-run prints. `snapshot` is what apply writes before its first mutation.
`diff` is dry-run, and also the last step of apply. `strip-owner-refs` is step 3 of apply,
surfaced by dry-run. Nothing was dropped; four commands became side effects of two modes.

**Pros**

- The equivalence diff is known **before** the CR is deleted. An unexpected difference aborts
  the run with nothing changed.
- Seeding is enforced (R3) and *derived*: the tool computes the values file from the live
  IntegrationConfig and prints it. The human copies a file, not a resource list.
- Ordering is in code, so §E's reversals cannot be re-litigated by a tired operator at 02:00.
- Idempotent by construction: every step is guarded by a read of the live state, so re-running
  after any failure resumes (R7) with no journal to corrupt.
- The prediction is exercised by the same fixtures as the chart's own equivalence test, so the
  tool and the chart cannot drift apart silently.

**Cons**

- The prediction is a *model* of template-operator-v2's renderer, not the operator. If tov2
  changes `SafeSprigFuncs`, or applies the rendered YAML differently than the tool parses it,
  dry-run is confidently wrong. Mitigated, not eliminated: `engine.go`'s doc comment already
  owns this risk for the render tests, `--apply` re-verifies against the live object as its
  last step, and the E2E lane runs the whole thing against a real tov2.
- Bigger than A. It needs the render engine, a diff classifier with a taxonomy, and a values
  emitter.
- Requires a Go toolchain to run (see Open Questions).

**Lifecycle scenarios** — the per-tenant walkthrough is the "Detailed design" section below,
so only the awkward ones are here.

*Interrupted between "delete CR" and "annotate Tenant".* The AppProject is unmanaged and
frozen; ArgoCD keeps honouring it, because an AppProject with no writer still works. Re-run:
the tool finds no Extensions CR, no opt-in annotation, and a live AppProject with no tov2
ownership labels, recognises the state as "CR drained, not yet adopted", skips straight to the
annotation patch, and finishes. This is the important scenario and the design is built for it.

*Interrupted between "annotate Tenant" and "child Ready".* Re-run: the tool finds the tenant
annotated and the child present, waits for `Ready`, and verifies. If the child is
`Ready=False` it prints the child's failing condition message — which, per
`templates/20-parent-template.yaml`, is the one that carries the real cause rather than the
`Ready` summary.

*The old controller is down when apply starts.* The liveness probe (R5) fails and the tool
refuses before deleting anything.

*Someone edits an AppProject between dry-run and apply.* Apply recomputes the prediction and
the diff from scratch; it never trusts dry-run output. The two runs disagreeing is itself the
signal, and apply's diff is the one that gates.

*A chart upgrade lands between dry-run and apply.* Same answer: apply re-reads the Template
objects, so it plans against the chart that is actually installed.

**Failure modes:** catalogued in full below.

**Complexity.** One command path with a per-tenant state machine, a prediction path reusing
the existing engine, a classifier, and a values emitter. Estimate 1100–1500 lines of Go
across ~6 files, no new direct dependency.

**Verdict:** Recommended.

### Alternative C: a chart-side `pre-upgrade` Helm hook Job

**Approach.** Ship the migration as a Job the chart runs before upgrade, so the operator's
existing `helm upgrade` does the whole thing.

**Verdict: Not viable**, and this is a recorded reversal (§E, E1) rather than a fresh
judgement. It needs an image in a chart that deliberately has none; a `pre-upgrade` hook
re-runs on *every* upgrade for ever; and it would delete `Extensions` CRs and patch
production `AppProject`s unattended, with its output going to a Job log nobody reads. The
ordering it was meant to enforce is already enforced by `scaffolding.mode: OnAnnotation`:
installing the chart adopts nothing until a human opts a tenant in.

### Alternative D: do nothing — hand-written runbook only

**Approach.** Write `MIGRATION.md` with `kubectl` commands and no tool. The steps are, after
all, a patch, a delete, two annotations and a wait.

**Pros.** Zero code, zero maintenance, nothing to keep in step with the chart. A competent
operator with the runbook can do it. And it is genuinely what the first tenant migration
already was — the 2026-08-07 `pe` run.

**Cons.** Every one of R1–R8 becomes a paragraph the human must read correctly. The seed
derivation is a jq incantation over the IntegrationConfig that the human must get right or
silently strip both resource lists from every AppProject on the cluster. There is no way to
see the equivalence diff before committing, because computing it requires rendering the
chart's gotemplates — which is the one thing a runbook cannot do. And the purge-flag trap
(R4) is a single missed field between "AppProject retained" and "AppProject and its
`jwtTokens` deleted".

**Verdict:** Not viable as the whole answer, but it is not wasted: MIGRATION.md is a
deliverable either way, and it must contain the manual break-glass sequence for the case
where the tool itself is the thing that is broken. So D ships *inside* B, as an appendix.

## Comparison matrix

| Criterion | A: five subcommands | **B: convergent cutover** | C: Helm hook Job | D: runbook only |
|---|---|---|---|---|
| Equivalence diff visible before mutation | No | **Yes** | No | No |
| Seeding enforced | Documented | **Refused if missing, values file emitted** | Would need chart values already right | Documented |
| R1 (no two writers) enforced by | Human ordering | **Code** | Job ordering | Human ordering |
| R4 (purge disarmed) enforced by | Human | **Code + observation gate** | Job | Human, one field |
| Resume after interruption | Re-run breaks on missing CR | **Converges** | Re-runs whole hook every upgrade | Human re-reads |
| Human commands per cluster | 5 + a hand-built values edit | **2, plus their own `helm upgrade`** | 1 (but unattended) | ~6 per tenant |
| Human commands per tenant | 1–2 | **0 with `--all`** | 0 | ~6 |
| New cluster-side objects | none | **none** | Job, SA, Role, image | none |
| Needs an image | No | **No** | Yes — disqualifying | No |
| Drift risk against the chart | Low (knows nothing about rendering) | **Medium — pinned by shared fixtures** | Medium | None |
| Testability | Command-level | **Unit + envtest + E2E + live dry-run** | Poor (hook-in-cluster) | None |
| Toolchain needed on the operator's laptop | Go | **Go** | none | kubectl + jq |
| Lines of code | ~700 | **~1300** | ~500 + image + CI | 0 |

## Recommendation

**Alternative B.** The decisive argument is not the command count — it is *when* the
equivalence check happens. In A, C and D the sequence is mutate-then-check, and the mutation
includes deleting the `Extensions` CR, which is the one step that cannot be undone without a
delete-and-recreate of the AppProject and the loss of `spec.roles[].jwtTokens`. B moves the
check in front of the mutation, and it can do that only because this repository already
contains a faithful in-process copy of template-operator-v2's renderer. That asset exists
because of the chart's own test strategy; spending it here is close to free.

The second argument is the seed. §E records that without carrying
`integrations.argocd.clusterResourceWhitelist` and `namespaceResourceBlacklist` into chart
values, "the diff is not empty by construction". A and D make that a sentence in a document.
B makes it a refusal with the fix attached — and, because the tool reads the IntegrationConfig
anyway to build the prediction, deriving the values file costs nothing extra.

The strongest counter-argument is B's own con: **the prediction can be confidently wrong.** A
dry-run that says "clean" and an apply that produces something different is worse than no
dry-run, because it manufactures false confidence. Three things answer it. The engine is not a
reimplementation but the same code the chart's tests pin, extracted to
`internal/gotemplate/`, so a divergence from tov2 breaks `make helm-render-test` before it
reaches a customer. Apply does not trust the prediction: its last step re-reads the live
AppProject and diffs *that*, so a wrong prediction surfaces as a loud post-apply failure on
one tenant rather than a silent fleet-wide one. And `--all` stops at the first tenant whose
post-apply verification fails, so the blast radius of a bad prediction is one tenant.

The choice is not close. What is close is whether `rollback` deserves to be a second
subcommand rather than a section of MIGRATION.md; I have kept it as a subcommand because it
has to *detect* the tov2 retain behaviour to choose its step order, and that detection is code.

## Detailed design (Alternative B)

### CRD changes

**None.** This plan adds no API, no CRD, no CRD version, and no field to any existing type. It
consumes three CRDs it does not own — `Extensions` (v1alpha1, cluster-scoped),
`Tenant` (v1beta3) and `IntegrationConfig` (v1beta1) — and two it does not own but the chart
installs objects of, `Template` and `TemplateInstance`. The only schema-adjacent artifact is a
vendored copy of the `Extensions` CRD under `tests/crds/` for envtest, which is test-only and
never applied to a real cluster.

The temptation worth naming and refusing: a `MigrationState` CR to record per-tenant progress.
It would make resume trivial and it is exactly the wrong trade. Progress is already legible
from the cluster — CR present or absent, annotation present or absent, child `Ready` or not —
so a state object would be a second source of truth that can disagree with the first, and it
would outlive the migration it exists for. R7 is satisfied by reading, not recording.

### Command surface

```text
mto-argocd-migrate cutover  (<tenant> | --all)  [--apply] [flags]
mto-argocd-migrate rollback (<tenant> | --all)  [--apply] [flags]
```

Two subcommands, and `--dry-run` is the default in both — `--apply` is the only way to write.
Flags, all with defaults that work on a normal cluster:

| Flag | Default | Purpose |
|---|---|---|
| `--context` | *required* | kube context. No fallback to `current-context` (R10); live-deployment.md records that the ambient context on the reference machine changes on its own. |
| `--apply` | off | Perform the mutations. Without it the tool is read-only apart from writing files under `--out-dir`. |
| `--all` | off | Every distinct `spec.tenantName` across all `Extensions` CRs, plus any tenant with a controller-shaped AppProject and no CR. Mutually exclusive with a positional tenant. |
| `--out-dir` | `./mto-argocd-migration` | Snapshots, the derived values file, and the run report. A per-run timestamped subdirectory. |
| `--continue-on-error` | off | With `--all`, keep going past a tenant that fails instead of stopping the run. |
| `--accept-diff` | off | Proceed despite an *unexpected* diff class. Prints what is being accepted. The escape hatch for a cluster whose AppProjects were hand-edited. |
| `--accept-gitops-drift` | off | Proceed on a tenant whose **`Tenant`** is ArgoCD-managed (`argocd.argoproj.io/tracking-id`). Without it those tenants are refused, because an auto-sync revert of the opt-in annotation is a measured delete-and-recreate of the AppProject. An ArgoCD-managed **`Extensions` CR** is only reported, not refused — removing it from Git in the same change is the documented procedure. |
| `--accept-token-loss` | off | Proceed on a tenant whose AppProject carries `spec.roles[].jwtTokens`, which adoption **destroys** (R2b). Separate from `--accept-diff` on purpose: an unexpected diff is a question, a destroyed credential is a decision. |
| `--skip-liveness-probe` | off | Skip R5. Documented with its exact consequence: the CR may wedge in `Terminating`, recoverable by clearing its finalizer, which is safe *because* purge is already false. |
| `--timeout` | `5m` | Per-tenant budget for the two waits (controller observation, child `Ready`). |

There is deliberately no `--yes`, no `--force` and no unattended mode.

### Preflight — once per run, before any tenant is touched

Fail-closed, in this order, because each check makes the next one meaningful.

1. **Context is explicit** and reachable; server version logged.
2. **Chart install found.** `Template/mto-argocd-parent` and
   `TemplateInstance/mto-argocd-parent` exist in the operator namespace, and the parent TI is
   `Ready=True`. The effective chart configuration is read from the parent Template's
   `spec.parameters[]`, which `templates/20-parent-template.yaml` bakes every value into as a
   literal — so the tool learns `argoNamespace`, `scaffoldingMode`, the four override
   annotation keys, `tenantNamespaceLabel`, `defaultWhitelistJson`, `defaultBlacklistJson`,
   `sourceReposPattern` and `defaultOnDeletePurge` from the cluster rather than from a local
   `values.yaml` that may not be what is installed. Both gotemplates are read from
   `spec.gotemplate` of the two Templates for the same reason.
3. **`scaffoldingMode` is `OnAnnotation` or `OnLabel`.** `Always` is refused: under `Always`
   every Tenant is *already* opted in, so R1 is already violated cluster-wide and the fix is a
   chart values change, not a migration. `Never` is refused because opt-in would do nothing.
4. **IntegrationConfig readable** at the chart's configured name/namespace, at `v1beta1`.
5. **`accessControl.privileged.namespaces` is sane** (R6): present, not null, no empty-string
   pattern, and every pattern compiles under Go's `regexp`. This is the fleet-killer check.
   The old controller's `IsPrivilegedNamespace`
   (`tenant-operator@main:pkg/utils/namespace/namespace_utils.go:22`) does
   `regexp.MatchString` and `continue`s on a compile error — so a bad pattern silently
   *disables* filtering for one entry and the cluster keeps working. The chart's parent uses
   `mustRegexMatch` and `fail`s on absence, null and empty patterns. So a cluster that is fine
   today can have a parent that refuses to render the moment the first tenant is opted in, and
   because tov2 holds the last rendered set when the parent cannot render, already-migrated
   tenants would freeze rather than break — the worst kind of failure to diagnose. Refuse
   before the first opt-in.
6. **tov2's sync-enforcement webhook carries its `matchConditions`** (TOV2-9). Checked exactly
   as `values.yaml` documents. Empty output means an unscoped, `failurePolicy: Fail` webhook on
   `*/*/*`, which is a dead cluster the moment tov2 is unreachable, and it does not self-heal.
   `hack/e2e/scope-tov2-webhook.sh` is the repair; the tool points at it and refuses.
7. **tov2 retain capability**, reported not enforced: is `deletion-policy: Orphan` honoured?
   Determined from the operator's chart version / image tag where legible, otherwise reported
   as unknown. Forward migration does not need it — the *old* controller's retain branch is
   what protects the AppProject during cutover — but `rollback` chooses its step order from it,
   and MIGRATION.md's warnings depend on it.
8. **Seed equality** (R3), three comparisons:
   - `argoNamespace` (chart) == `integrations.argocd.namespace` (IC). A mismatch is fatal and
     not fixable by seeding: the chart would create a *second* AppProject in a different
     namespace and leave the controller-owned one orphaned in the first.
   - `defaultWhitelistJson` (chart) == `integrations.argocd.clusterResourceWhitelist` (IC).
   - `defaultBlacklistJson` (chart) == `integrations.argocd.namespaceResourceBlacklist` (IC).

   On mismatch the tool writes `<out-dir>/migration-values.yaml` containing exactly the
   `argocd.*` block needed, prints the `helm upgrade --reuse-values -f` line, and exits
   non-zero **in both dry-run and apply**. This is the one human step the tool cannot take
   itself: chart values belong to Helm or to the GitOps repo, and a tool that ran
   `helm upgrade` behind ArgoCD's back would be immediately reverted.
9. **Tenant patch probe.** A `--dry-run=server` annotation patch against one Tenant at
   `tenantoperator.stakater.com/v1beta3`, discarded. It proves the API server accepts the
   write path the run ends with — which is what closes the conversion-webhook Unknown (TASKS
   A3(a)) without a real write.

### Per-tenant lifecycle

State is derived, never recorded. For tenant `t` the tool reads: every `Extensions` with
`spec.tenantName == t`; `Tenant/t` at v1beta3; the live `AppProject/t` in the ArgoCD
namespace; and `TemplateInstance/mto-argocd-tenant-t`. From those it classifies:

| CR(s) | Opt-in annotation | AppProject | Classification | Action |
|---|---|---|---|---|
| 1 | absent | present, no tov2 labels | **Standard.** The `pe` shape. | Full sequence below |
| 1 | absent | absent | Never synced | Delete CR, annotate; chart creates it fresh. No diff to check |
| 1 | **present** | present | **R1 violation, live now** | Refuse the run outright. Two writers exist; a human must decide which wins |
| 0 | absent | present, no tov2 labels | Orphaned AppProject — CR deleted earlier | Adopt only: strip refs, annotate. No CR step, no liveness probe needed |
| 0 | present | present, tov2-owned | Already migrated | Re-verify the diff; report; no writes |
| ≥2 | any | any | Ambiguous | Refuse this tenant. The controller's `filterExtensionsOnTenantName` returns the *first* match for namespace-triggered reconciles while each CR still reconciles itself on its own events, so which settings the live AppProject reflects is last-writer-wins and unknowable. A human resolves it |
| ≥1 | — | — | but `Tenant/t` does not exist | Report and skip. Nothing to opt in; the CR is orphaned |

The standard sequence, `--apply`:

1. **Snapshot** (R8). `<out-dir>/<run>/<tenant>/appproject.yaml` — the full live object,
   applyable as-is including `jwtTokens` — and `extensions-<name>.yaml`, the CR, stripped of
   `metadata.resourceVersion`/`uid`/`creationTimestamp` and of the
   `tenantoperator.stakater.com/appproject` finalizer so it re-applies cleanly. That second
   file is the input to `rollback`. If either write fails, the tenant is skipped with nothing
   done.
2. **Derive the per-tenant annotations** from the CR, and only those that differ from what the
   chart default would already produce:

   | CR field | Annotation | Rule |
   |---|---|---|
   | `spec.argoCD.appProject.sourceRepos` | `…/source-repos` | Non-nil → JSON array. Nil → no annotation, and since `sourceReposPattern` is empty the chart renders `[]`, matching `SourceRepos(nil)`. |
   | `…clusterResourceWhitelist` | `…/cluster-resource-whitelist` | Non-nil (**including an explicit `[]`**) → JSON array, because `populateArgoCDSettings` overrides on `!= nil`. Nil → no annotation, inheriting the seeded chart default. |
   | `…namespaceResourceBlacklist` | `…/namespace-resource-blacklist` | Same. |
   | `spec.argoCD.onDeletePurgeAppProject` | `…/on-delete-purge` | Written only when `true`, since `defaultOnDeletePurge` is `"false"`. Exactly `"true"`/`"false"` — the child template `fail`s on anything else, by design. |

   The `nil`-versus-`[]` correspondence is the pleasant part of this migration: Go's
   "override only when non-nil" and the chart's "absent inherits, `[]` overrides with empty"
   are the same rule expressed twice, so the mapping is total and needs no special cases.
3. **Predict.** Run the parent gotemplate over the live Tenants list with `t`'s annotations
   amended in memory (opt-in plus the derived overrides), the live Namespaces list, and the
   live IC `.spec`; take the child named `mto-argocd-tenant-t`; run the child gotemplate over
   its parameters. Result: the AppProject the chart will produce.
4. **Diff and classify** predicted against live. Three classes, and the class decides the
   verdict:
   - **Expected** — `spec.description`; `spec.roles[].groups` gaining exactly the tenant's
     users for that role, derived from `Tenant.spec.accessControl.*.users` rather than
     hardcoded; the chart's added `deletion-policy` annotation. Reported, does not block.
   - **Benign** — `spec.destinations` differing only in order (both sides build it from an
     API-server LIST, which carries no meaning) or by the removal of a duplicated ArgoCD
     namespace entry; a list field that is `[]` on one side and absent on the other, which
     ArgoCD reads identically. Reported, does not block.
   - **Unexpected** — anything else. Blocks this tenant unless `--accept-diff`.

   Classification is a pure function over two decoded objects, which is what makes it
   exhaustively unit-testable.
5. **Report the metadata work**: the fossil `v1beta2` Tenant ownerRef to strip if present;
   the `multi-tenant-operator/predicate` label that will *survive* adoption and why that is
   fine; the purge flag to disarm.
6. **Disarm purge, and prove the controller saw it** (R4, R5). One JSON merge patch on the CR
   setting `spec.argoCD.onDeletePurgeAppProject: false` *and* a throwaway label
   `mto-argocd-migrate/probe: <run-id>`. Then wait for
   `status.argocd.conditions[].lastTransitionTime` to advance past the patch time.

   Both halves of that patch are load-bearing. The spec change is needed because deleting a CR
   with purge armed deletes the AppProject and its `jwtTokens`. The label is needed because it
   is a trigger the controller's event filter accepts —
   `predicate.Or(GenerationChangedPredicate{}, LabelChangedPredicate{})` — so the probe works
   even for a CR whose purge flag was already false and whose generation therefore does not
   move. And the *wait* is needed because `Reconcile` reads the CR through controller-runtime's
   cache: a reconcile that started before the patch landed could hand `handleDeletion` a spec
   that still says purge. Waiting for a status write dated after the patch proves the
   controller has processed the new state, which makes the subsequent delete safe rather than
   merely likely. The same wait doubles as the liveness proof: if the controller is not
   running, the timestamp never moves and the tool refuses to delete a CR whose finalizer
   nothing would release.

   One gap, documented rather than papered over: if the IntegrationConfig has no
   `integrations.argocd` block, `syncArgoCDAppProject` returns before writing status, so the
   probe can never succeed. Preflight step 8 already refuses that cluster, because it needs
   `integrations.argocd.namespace` for the seed comparison. `--skip-liveness-probe` exists for
   the case where a cluster reaches that state anyway.
7. **Strip the fossil ownerRef.** A JSON patch removing only ownerRefs with
   `apiVersion: tenantoperator.stakater.com/v1beta2, kind: Tenant`, leaving every other
   ownerRef intact (§E, E1). Done **here**, before adoption: afterwards the chart owns the
   object and `sync.mode: strict` refuses the edit, so this ordering is not a preference. The
   old controller's own `removeOldOwnerReference` will not re-add it, so the strip is stable
   even if the controller reconciles again before step 8.
8. **Delete the Extensions CR**, controller still running. Wait for it to be gone — which
   means the finalizer was released.
9. **Confirm the AppProject survived** with the same UID (R2). If it is gone, the run
   **stops**: do not annotate, restore the AppProject from the step-1 snapshot, restore the CR,
   and exit non-zero. Opting a tenant in whose AppProject was just destroyed would paper over
   the very failure that must be investigated.
10. **One patch on the Tenant**: the opt-in annotation and every derived override annotation
    together. One patch, not two, so the parent never renders a child from a half-configured
    Tenant — no intermediate AppProject content, no extra write, no window where the tenant is
    opted in without its resource lists. Immediately before this patch the tool re-asserts R1
    by re-reading that no `Extensions` CR for `t` exists.
11. **Wait for `TemplateInstance/mto-argocd-tenant-t` to reach `Ready=True`.** On timeout or
    `Ready=False`, print the message from the condition with `status=False` that is *not*
    `Ready` — per `templates/20-parent-template.yaml`, `Ready` only ever summarises
    ("Template rendering failed") while the cause sits on `Rendered` or on the apply stage.
12. **Verify against reality.** Re-read the live AppProject. Assert the UID matches step 1's.
    Diff live against the step-3 prediction and classify again. Record that `jwtTokens` is now
    empty — it will be, by the mechanism in "Verified on a cluster", which is why step 0 below
    refuses rather than step 12 asserting. Write the per-tenant result into
    `<out-dir>/<run>/report.md`.

There is a step 0, and it comes before the snapshot: **read
`spec.roles[].jwtTokens` on the live AppProject and refuse the tenant if any are present**
(R2b), naming the roles and token ids. Adoption is a server-side apply of the atomic
`spec.roles` list, so the tokens are destroyed by the act of migrating and no ordering saves
them. This is the one place the design says "you cannot have this safely" instead of making it
safe, and the honest thing is to stop rather than to log a warning after the fact.

With `--all` this runs tenant by tenant, sequentially. Not in parallel: the parent
TemplateInstance re-renders on every Tenant event, so N concurrent opt-ins produce N parent
re-renders racing each other for no gain, and a sequential run keeps the report readable and
the failure attributable.

### The sequence, and who executes each step

The order **never changes**, on any cluster. What changes is *who* performs each step — the
tool, or a Git commit — and that is decided per object by who owns it. Five steps:

| # | Step | Owner-dependent? |
|---|---|---|
| 0 | `onDeletePurgeAppProject` → `false`, on every CR where it is `true` | Yes |
| 1 | The `Extensions` CR is gone | Yes |
| 2 | **Verify**: `AppProject` still there, same UID | No — always the tool, read-only |
| 3 | The `Tenant` carries the opt-in + override annotations | Yes |
| 4 | **Verify**: child `Ready`, diff is the two expected deltas | No — always the tool, read-only |

The rule for steps 0, 1 and 3 is one sentence: **whoever owns the object makes the change.** If
it is GitOps-managed, it is a commit; if it is applied by hand, the tool patches it. Getting
this wrong is not cosmetic — a tool patch to a GitOps-managed object is drift that ArgoCD
reverts, and for step 0 that revert re-arms purge just before step 1 deletes the CR, which
destroys the AppProject.

That gives four lanes. Preflight and the seed values (`migration-values.yaml`) are identical in
all four; only steps 0/1/3 move.

#### Lane A — Extensions by hand, Tenant by hand

The live cluster's shape, and the only lane tested end to end. The tool does everything:

```bash
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"            # dry-run
helm upgrade ... -f .../migration-values.yaml                              # once per cluster
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX" --apply    # steps 0-4
```

#### Lane B — Extensions in Git, Tenant by hand

Step 0 and step 1 go through Git, because a tool patch to the CR would be reverted and a tool
*delete* would simply be re-created by the next sync. Step 3 is the tool's.

```bash
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"   # reports which CRs are Git-managed

# commit 1 -- only for CRs where onDeletePurgeAppProject is true. Skip if none are.
#   edit: spec.argoCD.onDeletePurgeAppProject: false
git commit && git push      # wait for sync

# commit 2 -- remove the CR. ArgoCD's prune is the deleter, and it now retains.
git rm extensions/<tenant>-extensions.yaml && git push    # wait for sync

go run ./hack/mto-argocd-migrate cutover --all --context "$CTX" --apply
#   -> sees the CRs already gone, verifies each AppProject survived (step 2),
#      annotates the Tenants (step 3), verifies (step 4)
```

#### Lane C — Extensions by hand, Tenant in Git

The reverse split, and the one with a real trap: **the opt-in annotation must not be committed
before the CR is gone**, or ArgoCD opts the tenant in while the controller still owns the
AppProject — two writers, which is R1.

```bash
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX" --apply
#   -> does steps 0-2: disarms purge, deletes the CRs, verifies the AppProjects survived.
#      Then STOPS at step 3, because the Tenants are ArgoCD-managed, and prints the exact
#      annotation block to commit for each tenant.

# commit -- paste the printed annotations onto each Tenant
git commit && git push      # wait for sync

go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"
#   -> re-run, read-only: sees the annotations landed, waits for the children,
#      runs step 4 and reports the diff
```

The tool needs no new flag to do this: it is convergent by design (R7), so "do what I own,
report what I do not, and pick up where I left off on the next run" is the behaviour it already
has. The window between step 1 and step 3 is safe — an AppProject with no writer is still
honoured by ArgoCD, verified when the chart install was shown to adopt nothing.

#### Lane D — both in Git

Three commits, in this order, with a tool verify between each. Do **not** collapse commits 2
and 3 into one: ArgoCD applies a sync by kind and wave, so the ordering of a Tenant patch
against an `Extensions` prune inside one sync is not guaranteed, and the wrong order is two
writers.

```bash
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"     # dry-run: the plan

# commit 1  onDeletePurgeAppProject: false   (skip if no CR has it true)
# commit 2  git rm the Extensions CRs
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"     # verify step 2 for every tenant
# commit 3  the opt-in + override annotations on the Tenants
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"     # verify step 4
```

Note that in Lane D the tool never writes at all — `--apply` is not used, and every mutation is
a commit. It is a preflight-and-verify instrument there, which is the right shape: a GitOps
cluster should not have a CLI reaching in.

#### Mixed fleets

The lane is decided **per tenant**, not per cluster, from `argocd.argoproj.io/tracking-id` on
that tenant's `Tenant` and `Extensions` objects. So `--all` on a mixed cluster migrates the
Lane A tenants outright, and stops-and-reports on the others. Its summary groups tenants by
lane, so one dry-run tells you exactly which commits you owe.

### What a real operator actually types

Three commands, of which one is their own deployment path:

```bash
CTX=default/160-79-117-160:6443/ayesha.arshad@stakater.com

# 1. Read-only. Prints the fleet plan, the per-tenant classified diff, and writes
#    ./mto-argocd-migration/<run>/migration-values.yaml if seeding is missing.
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX"

# 2. Seed the chart. Their own Helm or GitOps path; the values file is generated.
helm upgrade mto-argocd oci://ghcr.io/stakater/charts/mto-extension-argocd \
  -n stakater-tenant-operator --reuse-values \
  -f ./mto-argocd-migration/<run>/migration-values.yaml

# 3. Do it.
go run ./hack/mto-argocd-migrate cutover --all --context "$CTX" --apply
```

On an already-seeded cluster step 2 disappears and it is two commands. Per tenant it is zero:
`--all` is the normal path, and `cutover <tenant>` exists for a staged rollout where the
operator wants to migrate one tenant, watch it for a day, and continue.

Afterwards, two steps that are not this tool's job and belong in the release notes: scale
`extensions-argocd-controller` to zero, then — once `kubectl get extensions` is empty
cluster-wide — delete the CRD. In that order. Reversed, every remaining CR wedges in
`Terminating` and the CRD deletion hangs behind them (§E, E4).

### Owned resources and garbage collection

The tool creates no Kubernetes object and owns nothing. It deletes `Extensions` CRs, patches
`Extensions` CRs, patches `Tenant` annotations, and patches `AppProject.metadata.ownerReferences`.
Everything it produces is a file under `--out-dir`.

The garbage-collection reasoning that matters is about an object it does *not* own: the fossil
`v1beta2` `Tenant` ownerRef on the AppProject. Kubernetes GC honours ownerRefs and knows
nothing about `templates.v2.stakater.com/deletion-policy: Orphan`, so leaving that ref in
place would mean deleting the Tenant deletes the "retained" AppProject. Step 7 exists for
exactly that, and the E2E lane asserts it: delete the Tenant, and a migrated AppProject with
the ref stripped survives.

Finalizers, all three of them, and none added by this tool:

- `tenantoperator.stakater.com/appproject` on the **Extensions CR** — released by the old
  controller during step 8. The whole ordering of this migration exists because nothing else
  releases it.
- `templates.v2.stakater.com/template-instance` on the child TemplateInstance — tov2's, added
  during step 11.
- tov2's finalizer on the AppProject, which is why `sync.ignoreFields` must keep
  `metadata.finalizers` (see `values.yaml`): without it the strict webhook locks tov2 out of
  its own object.

A force-delete of the Extensions CR (`--grace-period=0 --force`) does not skip a finalizer, so
there is nothing extra to leak there. Clearing the finalizer by hand *does* skip the
controller's deletion handler — which, with purge already false, is exactly equivalent to the
retain branch. That is why `--skip-liveness-probe` is a survivable escape hatch and not a trap.

### RBAC delta

**No `+kubebuilder:rbac:` markers, and no change to the chart's RBAC.** This is a CLI run with
a human's kubeconfig, not a controller. The permissions the human needs:

| Verb | Resource | Scope | Why |
|---|---|---|---|
| get, list | `extensions.tenantoperator.stakater.com` | cluster | Discover what to migrate |
| patch, delete | `extensions.tenantoperator.stakater.com` | cluster | Disarm purge; drain the CR |
| get, list, patch | `tenants.tenantoperator.stakater.com` | cluster | Read access control; write annotations |
| get | `integrationconfigs.tenantoperator.stakater.com` | ArgoCD/operator ns | Seed derivation, privileged lists |
| list | `namespaces` | cluster | Destination computation, mirroring the chart's own parent |
| get, patch | `appprojects.argoproj.io` | ArgoCD ns | Diff; strip the fossil ownerRef |
| get, list | `templates`, `templateinstances` `.templates.v2.stakater.com` | operator ns | Read the installed chart config and the two gotemplates; wait on the child |
| get | `validatingwebhookconfigurations` | cluster | The TOV2-9 preflight |
| get, list | `deployments` | MTO's ns | Secondary liveness signal on `extensions-argocd-controller` |

A cluster-admin has all of these. The security note worth making is the *opposite* of the
usual one: because this is a human-run CLI, nothing here grants a standing permission to any
ServiceAccount, and no permission outlives the migration. Alternative C would have needed a
ServiceAccount with delete on `Extensions` and patch on `AppProject`s, permanently, in every
cluster the chart is installed into.

### Observability

There is no controller, so there are no metrics and no events. Observability here is the
run's own output, and it has three consumers: the operator watching it, the report file
somebody reads after an incident, and CI.

- **Stdout** is a per-tenant block: classification, the planned steps, the classified diff, and
  a verdict line. Fleet summary table at the end. Nothing is elided at fleet scale — the
  per-tenant blocks are the point.
- **Exit codes** are the CI contract: `0` all tenants migrated (or already migrated), `1`
  preflight refused, `2` at least one tenant blocked by an unexpected diff, `3` at least one
  tenant failed mid-apply — which is the one that means "go look at the cluster now".
- **`<out-dir>/<run>/report.md`** — one row per tenant with UID before/after, the diff
  classes, the annotations written, and timings. This is the artifact that replaces
  live-deployment.md's hand-written "Migration, verified end to end" table for future runs.
- **Per-tenant snapshots** are the rollback input, and are also the evidence.
- **Existing cluster-side observability is what it always was**: the child TemplateInstance's
  conditions per tenant, and the parent's `status.outputs` roll-up for the fleet. The tool
  prints the same `jq` recipes live-deployment.md documents when a wait times out, rather than
  inventing a parallel vocabulary.

### Failure mode catalogue

| # | Trigger | Detection | Surfaced via | Recovery | Blast radius |
|---|---|---|---|---|---|
| 1 | Seed values missing | Preflight 8 | Refusal + generated `migration-values.yaml` | `helm upgrade`, re-run | None — nothing was touched |
| 2 | `argocd.namespace` ≠ IC's `integrations.argocd.namespace` | Preflight 8 | Refusal, marked unfixable-by-seeding | Human decides which namespace is right | None |
| 3 | IC `privileged.namespaces` absent/null/empty-pattern/uncompilable | Preflight 5 | Refusal quoting the offending pattern | Fix the IC | None — and this is the check that prevents a fleet-wide freeze |
| 4 | tov2 webhook unscoped (TOV2-9) | Preflight 6 | Refusal pointing at `hack/e2e/scope-tov2-webhook.sh` | Patch `matchConditions` | None — but skipping the check risks the whole cluster |
| 5 | `scaffoldingMode: Always` | Preflight 3 | Refusal | Set `OnAnnotation` and re-run | None; but R1 is *already* violated on that cluster |
| 6 | Tenant already annotated **and** its CR exists | Per-tenant classification | Refusal of the whole run | Human picks a writer | None added; existing overlap surfaced |
| 7 | ≥2 Extensions CRs for one tenant | Per-tenant classification | That tenant skipped, run continues | Delete the redundant CRs | One tenant not migrated |
| 8 | Unexpected diff class | Step 4 | Tenant blocked, diff printed | Fix the input, or `--accept-diff` | One tenant not migrated |
| 9 | Old controller down | Step 6 probe timeout | Refusal before the delete | Start the controller | None |
| 10 | Controller alive but status writes failing | Step 6 probe timeout | Refusal (false negative, fail-closed) | Investigate; `--skip-liveness-probe` with eyes open | None |
| 11 | CR wedges in `Terminating` after delete | Step 8 timeout | Failure with the recovery command | Clear the finalizer by hand — safe, purge is already false | One tenant, mid-flight |
| 12 | **AppProject gone after CR delete** | Step 9 | Run **stops**; tenant not annotated | Restore from snapshot (UID changes; `jwtTokens` lost) | One tenant, and it is the worst case in this document |
| 13 | Tenant patch rejected (conversion webhook, A3a) | Step 10 — and pre-empted by preflight 9's server-dry-run | Failure; CR already deleted, AppProject unmanaged but intact | Fix the webhook; re-run — the tool resumes at step 10 | One tenant, ArgoCD still working |
| 14 | Child never goes `Ready` | Step 11 timeout | Failure printing the non-`Ready` `status=False` condition | Fix the cause; re-run resumes at step 11 | One tenant. The pre-existing AppProject is still there and still honoured |
| 15 | Post-apply diff ≠ predicted diff | Step 12 | Failure; `--all` stops | Investigate the prediction; the AppProject is chart-managed and correct-or-not on the cluster | One tenant, and it caps the blast radius of a wrong prediction |
| 16 | UID changed across the run | Step 12 | Failure, loudly | Restore from snapshot | One tenant |
| 16a | **AppProject carries `jwtTokens`** | Step 0 | Refusal naming the roles and token ids | Re-mint the tokens after migration, or `--accept-token-loss` | One tenant, refused before anything is touched |
| 16b | **`jwtTokens` minted later are destroyed by the next re-render** | Not detectable by this tool — it is a steady-state chart defect | Nothing surfaces it; the token silently stops working | Re-mint, until the chart is fixed (first Open Question) | Every tenant, for ever, independent of migration |
| 17 | Snapshot cannot be written | Step 1 | Tenant skipped | Fix the path or `--out-dir` | None |
| 18 | Prediction engine drifts from tov2 | `make helm-render-test` in CI, and step 12 live | Test failure pre-release; per-tenant failure in production | Re-sync `internal/gotemplate` | Bounded to one tenant by step 12 + sequential `--all` |
| 19 | tov2 down mid-run | Step 11 timeout | Failure | Re-run when it is back | One tenant; and under `strict` its webhook is failing closed on managed AppProjects meanwhile |
| 20 | Two operators run `--apply` at once | Not detected | — | Don't. Documented, not enforced | Interleaved writes on one tenant. See Open Questions |

### Regression risk and blast radius beyond the change

**Other controllers affected.** In `tenant-operator@main`, `git grep -l AppProject` over
non-test Go returns `api/v1alpha1/extensions_types.go`,
`api/v1alpha1/integrationconfig_types.go`, `api/v1beta1/integrationconfig_types.go`,
`api/v1beta2/tenant_types.go`, `pkg/builders/appproject.go`, `pkg/objects/k8sobjects.go`,
`pkg/constants/metadata.go` and the two extensions-controller files. Of those, only the
extensions controller *acts* on an AppProject; the rest are type definitions and helpers. So
draining an `Extensions` CR removes the only tenant-operator writer of that tenant's
AppProject, and no other MTO controller starts caring about the surviving
`multi-tenant-operator/predicate` label.

The one watch that looked like it might matter does not. `DatabaseReconciler`
(`internal/controller/database/database_controller.go:74`) does
`Watches(&v1alpha1.Extensions{}, &handler.EnqueueRequestForObject{})` — but its `Reconcile`
body is `log.Info(...); return ctrl.Result{}, nil`. Deleting an `Extensions` CR therefore
costs one log line in the core manager and nothing else. TASKS F2 lists removing that watch as
part of Phase 3 cleanup; it is not a migration hazard.

**Inside this repository, the surface touched is small but not empty.** Moving
`tests/render/engine.go` to `internal/gotemplate/` is the only change to existing code. It has
about twenty call sites across `harness.go`, `chart_test.go`, `property_test.go`,
`hardening_test.go` and `equivalence_test.go`. Rather than requalify all of them, `tests/render`
keeps a three-line shim — `type Instance = gotemplate.Instance` plus two function values — so
the diff in the chart PR's test files is three lines, not twenty. That is deliberate: this PR
stacks on `feat/chart-scaffold-and-render-tests`, and a twenty-site rename would collide with
any late change to those files.

**External contracts changed.** None. No CRD field, no status condition, no event reason, no
metric. The `Extensions` CR is *deleted* rather than changed, so nothing that reads its status
survives to be broken — but note that `Extensions.status.argocd.appProjectState` is a thing a
customer could have alerted on, and after migration it is gone. The equivalent is the child
TemplateInstance's `Ready` condition, and live-deployment.md already tabulates the mapping.
That belongs in MIGRATION.md's release-note section.

**Tests that may fail for legitimate reasons.** `make test` today runs `helm-lint`, `vet`,
`helm-render-test` and `admission-test`. `vet` covers `./...`, so it will start covering
`hack/mto-argocd-migrate` and `tests/migrate` — including the `gofmt -l .` gate, which fails
the build on an unformatted new file. `helm-render-test` should be unaffected by the engine
move, and if it is not, the shim is wrong and that is exactly the failure I want. No existing
test asserts anything about migration, so nothing should fail for a *behavioural* reason.

**Tests that may fail for illegitimate reasons.** `tests/e2e` is the risk. Adding a migration
E2E to the kind lane means an in-process stub of the old controller runs against the same
cluster as `tests/e2e`'s existing fixtures. If the stub is not scoped to its own tenant it will
reconcile `e2e-alpha`/`e2e-beta`/`e2e-gamma`, write AppProjects under them, and make
`TestManagedFleet` — which relies on `e2e-beta` staying un-annotated — flap in a way that looks
like a chart bug. Mitigations, both required: the stub filters on its own fixture tenant names
only, and the migration E2E lives in its own file with its own fixture tenants
(`e2e-migrate-*`), created and torn down by itself. `cluster.go`'s `fixtureNamespaces` map and
`resetCluster` are the existing machinery to extend, not to bypass.

**New tests required specifically to catch unintended side effects**, beyond the ones that
prove migration works:

- The engine move is behaviour-neutral: `make helm-render-test-regenerate` must produce **zero**
  diff in `tests/golden/`. That is the regression guard for the refactor, and it is cheap.
- R1 as a property, not a happy path: inject a failure at step 8 (CR delete) and assert the
  Tenant was **not** annotated. Then inject at step 10 and assert the CR is **still** absent —
  i.e. the tool never walks the sequence backwards.
- A test that the tool refuses when a Tenant is already annotated *and* its CR exists
  (failure mode 6), because that is the one pre-existing-overlap shape a customer could
  present and the wrong answer is "helpfully fix it".
- An assertion that the AppProject's `resourceVersion` is unchanged by a `--dry-run` over every
  fixture tenant. Dry-run being genuinely read-only is a claim, and claims get tested.

**Untested code paths in the same area.** The two things live-deployment.md explicitly records
as uncovered are the purge-true branch and a tenant with per-tenant overrides. Both are now
covered by the envtest plan below — the stub controller can be handed a purge-armed CR without
risking a production AppProject, which is precisely why the stub exists.

### Upgrade and migration

Recursive but real: what happens to this tool across chart upgrades. Because it reads the
installed `Template` objects rather than a local `values.yaml`, a chart upgrade that renames a
parameter or edits a gotemplate is picked up automatically, and a chart upgrade that renames
the *child TemplateInstance prefix* breaks step 11's wait. The prefix `mto-argocd-tenant-` is
read from `templates/_helpers.tpl`'s convention today; the tool should derive the child name by
running the parent gotemplate and reading the emitted `metadata.name` rather than by string
concatenation. That makes the wait robust against exactly the rename that §"When an AppProject
gets deleted" flags as a breaking change.

Old and new operator coexistence: not applicable, there is no operator. The old *controller*
and the chart coexist for the whole duration of the migration, and that is the design — R1 is
about a single AppProject, not about the two implementations being simultaneously installed.
The overlap is per-tenant and it is empty by construction.

### Rollback plan

Two rollbacks, at different granularities.

**Rolling back the tool itself** is deleting a directory. It ships no cluster state.

**Rolling back a migrated tenant** is `rollback <tenant> --apply`, and its step order depends
on tov2's retain behaviour, which is why it is code and not a runbook paragraph:

*tov2 honours retain (`8a44194` / v0.0.7 and later)* — the live cluster, as of the 2026-08-21
refresh:

1. Remove the opt-in and override annotations from the Tenant. The child is reaped and the
   AppProject is **released** — same UID, tov2's finalizer and ownership labels stripped
   (verified 2026-08-21 by descaffolding `team-a`).
2. Re-apply the Extensions CR from the snapshot. The controller adopts the surviving object
   via `CreateOrPatch` — same UID, and `spec.roles[].jwtTokens` intact.

This order never has two writers and never has an absent AppProject. **If it holds, rollback
stops being destructive** — which contradicts what live-deployment.md records, because that
measurement was taken on v0.0.5 where step 1 *deleted* the object. It is listed in Unknowns and
must be proven on the kind lane before MIGRATION.md claims it.

*tov2 does not honour retain (v0.0.6 and earlier)* — reversed, because step 1 above would
destroy the object:

1. Re-apply the Extensions CR. The controller error-loops against the strict webhook —
   expected, and reproduced verbatim on 2026-08-07.
2. Remove the annotations. The child is reaped, the AppProject is deleted, and the
   already-retrying controller recreates it within seconds.

Measured cost, 2026-08-07: **~8 seconds with no AppProject**, and a **changed UID** — so
`spec.roles[].jwtTokens` are destroyed. Content otherwise identical to the pre-migration
snapshot: `0 changed, 0 new, 0 missing`. This is delete-and-recreate, not restore, and
MIGRATION.md must say so in those words. The self-healing in step 2 works *because* the
controller is already in a retry loop; a controller that had gone quiet might not notice for a
long time, so `rollback` waits for the AppProject to reappear rather than assuming it.

In both orders the tool re-diffs against the snapshot at the end and reports UID before/after,
so which of the two worlds the cluster is in is a measurement, not a belief.

**Rolling back the whole fleet** is `rollback --all`, plus reverting the chart's seed values
if they were added. Note what cannot be rolled back: a deleted `Extensions` CRD. That is why
the release-note order is scale-down-then-delete-CRD, and why deleting the CRD should wait
until every tenant has been migrated *and* observed.

### Scale and performance

Cluster reads are O(1) in tenant count, not O(N): one LIST of `Extensions`, one of `Tenant`,
one of `Namespace`, one GET of the IntegrationConfig, one GET of each of the two Templates —
then per tenant one GET of the AppProject and one of the child TemplateInstance. The parent
gotemplate is re-executed per tenant, which is O(N²) in template executions, but the input is
a few hundred KB and the execution is milliseconds; at 50 tenants that is 2500 executions of a
template that renders in under a millisecond. Not worth optimising, and worth stating so the
next reader does not.

Writes are three per tenant (one CR patch, one CR delete, one Tenant patch) plus at most one
AppProject patch. At 50 tenants that is ~200 writes over a sequential run whose wall clock is
dominated by two waits per tenant. Each Tenant patch triggers one parent re-render, which is
one write per opted-in child — the same cost as opting a tenant in by hand, which
live-deployment.md measures as "one tenant edit costs one write, not N".

The tail latency is the child `Ready` wait. Budget `--timeout 5m` per tenant; observed on the
live cluster the child reconciles in seconds. A 50-tenant `--all` should be minutes, and if it
is not, the parent is the bottleneck and the report's timings will say so.

### Threat model

The tool adds no cluster-side attack surface: no webhook, no ServiceAccount, no Deployment, no
token, no network listener. It runs with the operator's own credentials and exits. Compared
with Alternative C, which would have left a ServiceAccount permitted to delete `Extensions`
and patch `AppProject`s standing in every cluster for ever, that is the security argument for
this shape.

What the tool *trusts*, and why that is justified:

- **The gotemplates it reads off the cluster.** It executes `spec.gotemplate` from
  `Template/mto-argocd-parent` and `Template/mto-argocd-child` in the operator's local process.
  Anyone who can write those objects can already make template-operator-v2 render arbitrary
  AppProjects with tov2's own RBAC, so this is not new authority — but it is worth naming that
  a hostile Template becomes local code execution in the migrating operator's shell, with the
  operator's kubeconfig. Mitigated by the engine itself: `safeSprigFuncs` deletes `env`,
  `expandenv` and `getHostByName`, so a template cannot read the operator's environment or
  make it perform DNS lookups. That deletion was made for the operator's sake; it protects the
  CLI for free.
- **Tenant-controlled strings.** Annotations and access-control lists come from Tenants, which
  tenant authors edit. The tool only compares and re-emits them; the quoting that stops a
  group named `ok\n - injected-admin-group` from becoming two list entries lives in the child
  gotemplate and is already hardened and tested. The tool must not build YAML by string
  concatenation anywhere — it decodes to `map[string]any` and diffs structurally, which makes
  the injection class inapplicable rather than defended.
- **The snapshot directory.** It contains full AppProjects including `spec.roles[].jwtTokens`,
  which are **bearer credentials for ArgoCD project roles**. That is a real secret on the
  operator's laptop. The tool must create `--out-dir` with mode `0700`, files `0600`, say so on
  stdout, and MIGRATION.md must tell the operator to delete the directory when the migration is
  confirmed. This is the one genuinely new secret-handling surface in the plan and it should
  not be discovered by a reader later.

### GitOps considerations

The tool writes three things a GitOps system may care about, and each needs an answer:

- **Tenant annotations. Measured, and it is destructive.** If Tenants are managed in Git, the
  tool's annotation patch is drift and ArgoCD reverts it. That does not merely un-migrate the
  tenant: on 2026-08-23, removing the opt-in annotation **deleted `AppProject/pe` within 2
  seconds**, and re-adding it recreated the object with a **new UID**
  (`03fe60a9` → `d30ea975`). So one auto-sync cycle is a delete-and-recreate — new UID, tokens
  gone, and a window in which ArgoCD refuses every Application in the project. That was tov2
  v0.0.6, where retain is not enforced (TOV2-1); on v0.0.7 the same step should *release* the
  object and re-adopt it in place, which is the difference between an outage and a no-op and is
  still listed in Unknowns.

  **This is the sharpest hazard in the plan, and it earns a refusal rather than a warning** —
  the earlier draft proposed warning, and the measurement changed the answer. If the Tenant
  carries `argocd.argoproj.io/tracking-id`, or is otherwise owned by an Application, `--apply`
  **refuses** that tenant. MIGRATION.md's instruction: commit the annotations to Git first and
  let them land, after which the tool's patch is a no-op it detects and skips; or migrate with
  auto-sync disabled and commit immediately afterwards. `--accept-gitops-drift` exists for the
  operator who has genuinely disabled auto-sync and knows it.
- **The seed values.** Already handled by refusing to run `helm upgrade` itself; the values
  file is meant for the operator's Git repo.
- **Extensions CR deletion — a sequencing note, not a hazard.** The obvious procedure removes
  the CR from Git in the same change that migrates the tenant, so ArgoCD has nothing to
  recreate. No refusal is warranted and an earlier draft of this plan was wrong to propose one:
  designing around an operator who deletes the CR from the cluster but leaves it in Git is
  defending against a mistake the runbook already prevents.

  What survives is an **ordering constraint, and it makes R4 more load-bearing rather than
  less.** If the CR is GitOps-managed, ArgoCD's prune is the deleter, not this tool — and an
  ArgoCD delete sets a `deletionTimestamp` like any other, so the controller's finalizer runs
  `handleDeletion` and reads `onDeletePurgeAppProject` at that moment. So the operator no longer
  controls *when* the delete happens:

  ```bash
  # WRONG: ArgoCD prunes the CR while purge is still armed -> AppProject destroyed
  git rm extensions/pe-extensions.yaml && git push
  go run ./hack/mto-argocd-migrate cutover pe --apply   # too late, nothing to adopt

  # RIGHT: disarm and drain first; the Git change is then a no-op on the cluster
  go run ./hack/mto-argocd-migrate cutover pe --apply
  git rm extensions/pe-extensions.yaml && git push
  ```

  So: dry-run **reports** when a CR carries `argocd.argoproj.io/tracking-id`, and prints that
  order. It does not refuse. And if the CR *does* come back before the Git change lands, the
  measurement above is the safety net — the returning controller's write and delete are both
  refused by the strict webhook and the AppProject does not move; the only cost is a controller
  in a retry loop until Git catches up.

The chart's own GitOps story is unchanged and already handled: the RBAC carries
`argocd.argoproj.io/sync-options: Prune=false,Delete=false`, per `NOTES.txt`.

### Multi-tenancy

The unit of work is the tenant, and tenants are isolated by construction: one `Extensions` CR,
one `Tenant`, one AppProject, one child TemplateInstance, all keyed by tenant name. A failure
on tenant A cannot corrupt tenant B's AppProject, because no step writes an object shared
between them — the one shared object is the parent TemplateInstance, and the tool never writes
it.

The two genuine cross-tenant hazards both live in the chart, not the tool, and both are
already handled — and both are why preflight is fail-closed rather than best-effort. A broken
IntegrationConfig `privileged.namespaces` list would put one tenant's privileged namespaces
into another tenant's destinations (cross-tenant exposure), which is preflight 5. And a
malformed override annotation fails one child only, because the parent passes annotation values
through verbatim and the child parses them — so one tenant's bad JSON cannot take the fleet
down. The tool inherits that property: it derives annotations per tenant and writes them per
tenant.

Two `Extensions` CRs naming one tenant is the multi-tenancy edge case that does exist, and the
tool refuses it rather than guessing (classification table, row 6).

## Examples and walkthroughs

### The input: what a real before-state looks like

This is the live `pe` shape, from `docs/plans/spike-argocd-tov2/fixtures/` and
live-deployment.md. The CR:

```yaml
apiVersion: tenantoperator.stakater.com/v1alpha1
kind: Extensions
metadata:
  name: pe-extensions
spec:
  tenantName: pe
  # No argoCD block at all -- the nil-config retain branch. This is the shape
  # that made the 2026-08-07 run safe, and the shape that leaves the purge
  # branch untested.
```

The Tenant:

```yaml
apiVersion: tenantoperator.stakater.com/v1beta3
kind: Tenant
metadata:
  name: pe
spec:
  quota: small
  accessControl:
    owners:
      users: [ayesha.arshad@stakater.com]
  namespaces:
    withTenantPrefix: [payments, batch]
```

And the IntegrationConfig's deprecated block, which is the whole seeding problem in six lines:

```yaml
spec:
  integrations:
    argocd:
      namespace: stakater-tenant-operator
      clusterResourceWhitelist:
        - {group: tronador.stakater.com, kind: EnvironmentProvisioner}
        - {group: "", kind: PersistentVolume}
      namespaceResourceBlacklist:
        - {group: "", kind: ResourceQuota}
```

### Mode 1 — dry run on an unseeded cluster

```bash
go run ./hack/mto-argocd-migrate cutover --all \
  --context default/160-79-117-160:6443/ayesha.arshad@stakater.com
```

```text
preflight
  context      default/160-79-117-160:6443/... (OpenShift 4.20, k8s v1.33.5)
  chart        mto-argocd-parent  Ready=True   argoNamespace=stakater-tenant-operator
               scaffolding.mode=OnAnnotation   onDeletePurgeAppProject=false
  tov2         v0.0.7  retain=honoured  sync-enforcement matchConditions=present
  int.config   tenant-operator-config/stakater-tenant-operator
               privileged.namespaces: 14 patterns, all compile, none empty     OK
  seed         argocd.namespace                      OK
               argocd.clusterResourceWhitelist       MISSING (chart: [], IC: 2 entries)
               argocd.namespaceResourceBlacklist     MISSING (chart: [], IC: 1 entry)

REFUSING: the chart is not seeded. Every migrated tenant would lose both resource
lists from its AppProject. Wrote the values needed:

  ./mto-argocd-migration/2026-08-23T12-40-11Z/migration-values.yaml

  helm upgrade mto-argocd <chart> -n stakater-tenant-operator --reuse-values \
    -f ./mto-argocd-migration/2026-08-23T12-40-11Z/migration-values.yaml

Then re-run this command. Nothing was changed.
exit 1
```

The generated file is exactly what the live install already carries by hand today:

```yaml
# Derived from IntegrationConfig tenant-operator-config/stakater-tenant-operator
# .spec.integrations.argocd on 2026-08-23. Deprecated there; the chart does not
# read it, so it must be carried into values or every AppProject loses both lists.
argocd:
  clusterResourceWhitelist:
    - {group: tronador.stakater.com, kind: EnvironmentProvisioner}
    - {group: "", kind: PersistentVolume}
  namespaceResourceBlacklist:
    - {group: "", kind: ResourceQuota}
```

### Mode 2 — dry run on a seeded cluster

```text
preflight  ... all OK

tenant pe                                          [standard]
  extensions CR   pe-extensions  (purge: false -- nil argoCD block)
  tenant          pe             owners.users=[ayesha.arshad@stakater.com]
  appproject      pe             uid=8bff29cd-...  11 destinations
  child           mto-argocd-tenant-pe   (absent -- not yet opted in)

  will write on Tenant/pe, one patch:
    argocd.tenantoperator.stakater.com/enabled = "true"
    (no override annotations -- the CR sets none, so the chart defaults apply)

  will strip from AppProject/pe:
    (nothing -- no tenantoperator.stakater.com/v1beta2 Tenant ownerRef present)

  predicted diff, chart vs live:
    EXPECTED  spec.description
                - "Managed by Extensions controller"
                + "Managed by mto-extension-argocd"
    EXPECTED  spec.roles[0].groups  (+1: ayesha.arshad@stakater.com)
                  pe-owner-group-489fab
                + ayesha.arshad@stakater.com
    EXPECTED  metadata.annotations
                + templates.v2.stakater.com/deletion-policy: Orphan
    unchanged  destinations (11) · clusterResourceWhitelist (2)
               namespaceResourceBlacklist (1) · roles[].policies · sourceRepos (absent)

  verdict: SAFE TO APPLY  (3 expected, 0 benign, 0 unexpected)

fleet summary
  1 tenant  1 safe  0 blocked  0 already migrated
  re-run with --apply to migrate.
exit 0
```

The `roles[0].groups` line is the one to read twice: it is the documented deliberate delta,
and it is derived from `Tenant.spec.accessControl.owners.users`, not hardcoded — so a tenant
with three owners shows three additions and a tenant with none shows the line absent.

### Mode 3 — apply

```text
tenant pe                                          [standard]
  1  snapshot            appproject.yaml (4.1K, 0600)  extensions-pe-extensions.yaml
  2  derive annotations  1 (opt-in only)
  3  predict             ok
  4  diff                3 expected, 0 benign, 0 unexpected
  5  metadata plan       no ownerRef to strip
  6  disarm purge        already false; probe label set, controller observed in 1.8s
  7  strip ownerRefs     skipped (nothing to strip)
  8  delete CR           pe-extensions gone in 0.9s (finalizer released)
  9  appproject survived uid=8bff29cd-... UNCHANGED
 10  annotate tenant     1 patch, 1 annotation
 11  child ready         mto-argocd-tenant-pe Ready=True in 3.2s
 12  verify              uid unchanged · diff matches prediction exactly
                         jwtTokens byte-identical (0 tokens)

  MIGRATED  pe  (11.4s)

fleet summary
  1 migrated  0 failed  0 skipped
  report: ./mto-argocd-migration/2026-08-23T12-51-02Z/report.md

Next, and NOT done by this tool:
  1. kubectl scale deploy/extensions-argocd-controller --replicas=0 -n <mto-ns>
  2. once `kubectl get extensions` is empty: delete the CRD.
  In that order -- reversed, every remaining CR wedges in Terminating.
Delete ./mto-argocd-migration/... when you are satisfied: the snapshots contain
AppProject roles[].jwtTokens, which are ArgoCD bearer credentials.
exit 0
```

### Mode 4 — a tenant with overrides and purge armed

The shape live-deployment.md records as covered by nothing. Given:

```yaml
apiVersion: tenantoperator.stakater.com/v1alpha1
kind: Extensions
metadata:
  name: acme-extensions
spec:
  tenantName: acme
  argoCD:
    onDeletePurgeAppProject: true          # <-- the dangerous one
    appProject:
      sourceRepos: [https://github.com/acme/acme-gitops]
      clusterResourceWhitelist: []         # explicit empty: overrides the IC list
      # namespaceResourceBlacklist omitted: inherits the IC list
```

dry-run prints:

```text
tenant acme                                        [standard]
  extensions CR   acme-extensions  (purge: TRUE -- will be patched to false)

  will write on Tenant/acme, one patch:
    argocd.tenantoperator.stakater.com/enabled                      = "true"
    argocd.tenantoperator.stakater.com/source-repos                 = ["https://github.com/acme/acme-gitops"]
    argocd.tenantoperator.stakater.com/cluster-resource-whitelist   = []
    argocd.tenantoperator.stakater.com/on-delete-purge              = "true"
    (namespace-resource-blacklist: no annotation -- inherits the seeded default,
     matching the CR leaving it nil)

  WARNING  onDeletePurgeAppProject is true on acme-extensions. Deleting the CR
           as-is would DELETE AppProject/acme and its roles[].jwtTokens. --apply
           will patch it to false and confirm the controller has observed the
           patch before deleting anything.

  verdict: SAFE TO APPLY  (3 expected, 0 benign, 0 unexpected)
```

Note the two annotations that are *not* written and the one that is: `[]` is carried through
because `populateArgoCDSettings` overrides on `!= nil` and the chart overrides on
key-present, so an explicit empty list means the same thing on both sides; the omitted
blacklist is left to inherit; and `on-delete-purge: "true"` carries the tenant's *intent*
forward even though the flag is disarmed on the CR for the duration of the delete. That last
one is the subtle bit — the CR is patched to `false` only so the delete retains, while the
tenant's real preference lands on the Tenant annotation and governs from then on.

### Mode 5 — rollback

```bash
go run ./hack/mto-argocd-migrate rollback pe --context "$CTX" --apply
```

```text
tenant pe                                          [migrated]
  tov2 retain=honoured  ->  annotation-first order (never two writers, never absent)
  1  snapshot          current chart-managed AppProject/pe
  2  de-annotate       removed enabled + 0 override annotations
  3  child reaped      mto-argocd-tenant-pe gone in 2.1s
  4  appproject        RELEASED  uid=8bff29cd-... UNCHANGED
                       tov2 finalizer and ownership labels stripped
  5  restore CR        pe-extensions applied from
                       ./mto-argocd-migration/2026-08-23T12-51-02Z/pe/extensions-pe-extensions.yaml
  6  controller adopt  AppProjectSynced in 1.4s
  7  verify            uid UNCHANGED · 0 changed, 0 new, 0 missing vs snapshot
                       jwtTokens byte-identical

  ROLLED BACK  pe  (non-destructive: UID preserved)
```

On a cluster whose tov2 does not honour retain the same command prints the reversed order and
a blunter verdict:

```text
  tov2 retain=NOT honoured  ->  CR-first order
  WARNING  this rollback is a DELETE AND RECREATE, not a restore. AppProject/pe
           will be absent for ~8s and will come back with a NEW UID. Every
           spec.roles[].jwtTokens on it -- ArgoCD project role tokens minted by
           `argocd proj role create-token` -- is destroyed. 0 tokens found on
           AppProject/pe, so nothing is lost here; check again before running
           this on a tenant that uses them.
```

## Test plan

Four levels. The interesting property of this design is that the two things
live-deployment.md records as **untested** — the purge-true branch and a tenant with
overrides — become cheaply testable, because an in-process stub of the old controller can be
handed a purge-armed CR without risking a production AppProject.

**Unit — `tests/migrate`, no cluster.**

| Function | Cases |
|---|---|
| Seed derivation | IC with both lists · with one · with neither · `integrations` absent · `integrations.argocd` absent · `argocd` present but `namespace` empty (must refuse) · IC yielding a values file that round-trips through `helm template` unchanged |
| Preflight: privileged patterns | present · absent · explicit null · contains `""` · contains `[.*` (uncompilable) · contains `.*` (legitimate deny-all, must pass) |
| Annotation derivation | `argoCD` nil · `argoCD` present with nil `appProject` · `sourceRepos` nil/`[]`/populated · whitelist nil/`[]`/populated · blacklist same · purge true/false/absent · a CR setting all four |
| Diff classifier | one case per row of the field table in Context, asserting the class: description → expected · roles groups gaining exactly the tenant's users → expected · gaining anything else → **unexpected** · destinations reordered → benign · destinations with a duplicated argo NS collapsed → benign · destinations gaining or losing a real namespace → **unexpected** · `[]` vs absent → benign · a changed policy string → **unexpected** · a changed role name → **unexpected** |
| Prediction | Against `docs/plans/spike-argocd-tov2/fixtures/`: predicting from `input-tenant-pe.json` + `input-namespaces-pe.json` + `input-integrationconfig.json` must reproduce `golden-appproject-pe.json` with exactly the two deltas `tests/render/equivalence_test.go` already asserts. This is the test that keeps the tool and the chart from drifting apart, and it reuses the fixtures rather than copying them |
| Per-tenant classification | Every row of the classification table, including ≥2 CRs and the already-annotated-with-live-CR refusal |
| Plan ordering | Golden-file the generated step list for each input shape. The purge-armed CR must produce disarm-before-delete; no input may produce annotate-before-delete. This is R1 and R4 asserted structurally rather than behaviourally |

**Envtest — `tests/migrate`, real API server, no operators.** Extends the pattern in
`tests/admission`, which already stands up an API server via `setup-envtest` at k8s 1.33. CRDs:
the two already vendored in `tests/crds/`, plus `tenantoperator.stakater.com_extensions.yaml`
(vendors verbatim — single version, no conversion block), plus the AppProject CRD. A **stub
Extensions controller** runs in-process, reproducing exactly the branches read from
`extensions_argocd_controller.go`: add the finalizer; on deletion, purge only when
`ArgoCDConfig != nil && OnDeletePurgeAppProject`; write a status condition with a fresh
`LastTransitionTime` on every sync; the event filter
`Or(GenerationChangedPredicate, LabelChangedPredicate)`.

| Scenario | Assertion |
|---|---|
| Standard cutover, purge false | CR gone · AppProject present with the same UID · Tenant annotated · one patch, not two |
| **Purge armed** | The tool patches to false, waits for the status timestamp, then deletes — and the AppProject survives. Then the negative control: bypass the patch and assert the stub *does* delete it, proving the test is testing something |
| Controller not running (stub stopped) | Refuses to delete; CR untouched; Tenant not annotated |
| Controller running but status writes rejected | Refuses (fail-closed), CR untouched |
| Stale-cache race | Stub configured to serve one reconcile from a pre-patch snapshot; the tool's wait must not be satisfiable by that reconcile |
| Interrupt after CR delete | Re-run converges: annotation applied, no attempt to delete a missing CR |
| Interrupt after annotate | Re-run converges: waits for the child, does not re-patch |
| Fossil ownerRef | Present → stripped, other ownerRefs untouched · absent → no patch issued at all |
| Tenant deleted after migration, ref stripped | AppProject survives GC (the reason step 7 exists) |
| Tenant deleted after migration, ref **not** stripped | AppProject is garbage-collected — the negative control that justifies step 7 |
| ≥2 CRs for one tenant | Refused; nothing written |
| Already annotated + live CR | Whole run refused |
| Dry-run over every scenario | Zero writes: assert `resourceVersion` unchanged on every object touched |

**E2E — `tests/e2e`, kind, real tov2 + real ArgoCD.** Its own file and its own fixture tenants
(`e2e-migrate-*`), created and torn down by itself, extending `cluster.go`'s
`fixtureNamespaces`/`resetCluster` rather than bypassing them. The stub controller runs in the
test process, **scoped to its own tenants only** — otherwise it reconciles `e2e-beta` and makes
`TestManagedFleet` flap in a way that looks like a chart bug.

- Full cutover of a tenant with a stub-controller-created AppProject, through the real chart
  and real tov2: UID preserved, child `Ready`, diff matches the prediction.
- A tenant with all four overrides set on the CR — the second thing live-deployment.md records
  as uncovered.
- **`jwtTokens` survival**: mint a token into `spec.roles[].jwtTokens` before cutover (a plain
  patch, no ArgoCD CLI needed), assert it is byte-identical afterwards. This is the assertion
  that makes R2 mean something.
- ArgoCD honours the migrated project: an `Application` inside a destination is accepted, one
  outside it is refused — reusing the existing E2E assertion, but after a migration rather than
  after a fresh install.
- Rollback both ways, with the retain behaviour of the pinned tov2 recorded in the output. The
  kind lane is where the "is rollback destructive on v0.0.7?" Unknown gets closed, because it
  is the only environment where the operator version is controlled.
- Prediction fidelity as a property: for each fixture tenant, the tool's predicted AppProject
  must equal what tov2 actually applied. A failure here is the drift risk (failure mode 18)
  caught in CI rather than in production.

**Regression guards** (from the Regression risk section): the engine move must leave
`tests/golden/` byte-identical under `make helm-render-test-regenerate`; injected failures at
steps 8 and 10 must never leave the sequence walked backwards; dry-run must not move any
`resourceVersion`.

**CI wiring**, because a suite CI does not run is decoration. `make test` today is
`helm-lint vet helm-render-test admission-test`, and `.github/workflows/pull_request.yaml`
calls that one aggregate target precisely so the two cannot drift. So: a new `migrate-test`
target running the unit suite unconditionally, added to `test`'s prerequisites; the envtest
half gated behind `KUBEBUILDER_ASSETS` and **skipped loudly** when it is unset, exactly as
`tests/admission/admission_test.go` documents doing, and wired into the existing
`envtest-assets` path; the E2E half added to the `e2e-kind` lane. `vet` already covers
`./...`, so the new `hack/mto-argocd-migrate` package is gofmt- and vet-checked in CI from the
first commit — which also means an unformatted new file fails the build, not the next diff that
touches it.

**Live cluster.** `cutover pe --dry-run` against
`default/160-79-117-160:6443/ayesha.arshad@stakater.com` is the acceptance gate, and it is
read-only. It is the only place the real MTO, real OpenShift, real conversion webhooks and a
real controller-owned AppProject exist together. `--apply` there only on explicit approval,
and the tenant is `pe`, whose before-state is the customer shape. Everything else runs in kind.

## Rollout plan

The tool is not deployed, so "rollout" means the order in which it is used and trusted.

1. **Land the PR with the live dry-run recorded.** The acceptance evidence for review is the
   `pe` dry-run output pasted into MIGRATION.md, alongside the kind E2E run. Nothing on the
   live cluster is mutated to merge this.
2. **First real apply is one tenant, on the live cluster, watched.** `cutover pe --apply`.
   Watch: the child's `Ready` condition, the parent's `status.outputs.errors`, and ArgoCD
   accepting an Application under `pe`. Rollback trigger: any unexpected diff class, any UID
   change, or the child not `Ready` inside the timeout.
3. **Then `--all` on a cluster with more than one migratable tenant.** The live cluster cannot
   provide this — MTO's free tier caps it at two Tenants and `team-a` is already chart-managed
   — so the multi-tenant `--all` path is proven in kind and on the first customer cluster
   under supervision.
4. **Only then the release note.** Scale `extensions-argocd-controller` to zero; confirm
   `kubectl get extensions` is empty cluster-wide; delete the CRD. In that order (§E, E4).
   TASKS §F is gated on this being true everywhere, and the tool's report files are the
   evidence per cluster.

Communication: the behaviour change customers see is the two documented AppProject deltas, the
loss of `Extensions.status.argocd`, and — for anyone managing Tenants or Extensions CRs in Git
— a required commit. That is MIGRATION.md's release-note section, written to be copy-pasted.

## Open questions

- **Does the `jwtTokens` defect get fixed, and by whom?** This is now the biggest open
  question in the document, and it is **outside this PR's scope** — it is a chart/tov2 issue
  that migration surfaced, not creates. Measured cause: tov2 server-side-applies the atomic
  `spec.roles` list, so every re-render drops sub-fields the chart does not render. Three
  candidate fixes, none of them mine to choose:
  1. **Upstream tov2**: teach the applier to preserve declared-exempt paths — i.e. make
     `sync.ignoreFields` govern the *apply* as well as drift comparison. This is the correct
     fix and it is a new upstream issue (TOV2-10), because `ignoreFields` promising protection
     it cannot deliver is a defect in its own right.
  2. **Upstream argo-cd**: add `x-kubernetes-list-map-keys: [name]` and
     `x-kubernetes-list-type: map` to `spec.roles`, which would make SSA merge per-role and
     leave untouched sub-fields alone. Correct, slower, and not in Stakater's gift.
  3. **In this chart**: render `jwtTokens` back into the child by reading them off the live
     AppProject. Not possible — the gotemplate has no read of its own output, and adding one
     would need a new `objectFieldRef` per tenant.

  Until one lands, the honest position is R2b: refuse, name the tokens, and document it. Also
  note the old controller has the **same** defect, for the same reason and verified in code:
  `pkg/builders/appproject.go:143` is `b.obj.Spec.Roles = roles`, a wholesale assignment, and
  `addRoles` never builds a `JWTTokens` field — so `CreateOrPatch` fetches the live object,
  overwrites its roles with token-free ones, and patches. The old controller's triggers are the
  same ones (namespace events, IntegrationConfig changes), so the exposure is identical. **This
  is a pre-existing multi-tenant-operator defect faithfully reproduced, not a regression the
  chart introduces** — which is also why "byte-identical migration" remains an honest goal.
  Worth confirming before this gates anything: whether any customer is actually using ArgoCD
  project role tokens. If nobody is, R2b is a documentation line; if someone is, it gates the
  rollout — and they have been silently losing tokens already.
- **Should `values.yaml`'s `sync.ignoreFields` comment be corrected in this PR or the chart
  PR?** Its claim that "tokens survive every re-render regardless" is measurably false. It is a
  comment, not behaviour, so it is a safe edit — but it belongs logically to the chart PR.
  Proposed default: fix it here, in the same commit as the plan, and cross-reference the new
  upstream issue; the chart PR is already in review and the comment is actively misleading.
- **How does a customer without a Go toolchain run this?** Proposed default: they do not — for
  now the migration is performed by whoever installs the chart, from a checkout, with
  `go run`. If that is wrong, the fix is a `go build` matrix in `.github/workflows/push.yaml`
  attaching three static binaries to the GitHub release, which is maybe 30 lines and no new
  dependency. What would change the answer: any customer self-migrating rather than being
  migrated by SAAP.
- **Should `--all` be parallel?** Proposed default: no, sequential. Each Tenant patch triggers
  a parent re-render, so concurrency buys little and costs report legibility and failure
  attribution. What would change the answer: a cluster with hundreds of tenants where the
  per-tenant `Ready` wait dominates.
- **Should the tool refuse to run when two copies of itself are running?** Failure mode 20 is
  the only one with no detection. A lease or a lock annotation on the parent TemplateInstance
  would close it. Proposed default: document it and move on; this is a supervised one-shot
  operation, and a half-held lock is its own failure mode. What would change the answer: a
  plan to run it from CI.
- **Does `rollback` belong in this PR at all?** It doubles the E2E surface for a path that is
  used only when something has gone wrong. Proposed default: yes, keep it — the forward path's
  credibility depends on the rollback being tested rather than described, and §E's rollback
  claims are currently measured on an operator version the live cluster no longer runs.
- **Should the tool offer to remove the `multi-tenant-operator/predicate` label after
  adoption?** Proposed default: no. It is inert once the CR is gone (no non-extensions
  controller reads AppProjects), removing it is a write with no benefit, and it is a useful
  forensic marker for "this object predates the chart". Revisit if MTO ever adds a controller
  that acts on labelled objects generically.
- **Where does MIGRATION.md live relative to `docs/rollback.md`?** They overlap: `rollback.md`
  covers chart-managed installs and explicitly defers migration rollback to "a follow-up PR".
  Proposed default: `MIGRATION.md` at the repository root (as §E3 names it), owning the
  migration and its rollback, with `rollback.md` gaining a pointer. Also note `rollback.md`'s
  "Never `helm uninstall` on its own" contradicts the 2026-08-21 single-command uninstall in
  `NOTES.txt` and live-deployment.md — a pre-existing staleness this PR should fix while it is
  in the neighbourhood, or deliberately leave alone.

## Decisions made

- **One subcommand for the forward path.** `cutover (<tenant>|--all)`, `--dry-run` by default,
  `--apply` to mutate. `inventory`, `snapshot`, `strip-owner-refs` and `diff` are folded in as
  phases, not commands.
- **The dry run predicts.** The post-migration AppProject is computed in-process from the
  installed `Template` objects and live inputs, and diffed before anything is written.
- **A second subcommand, `rollback`**, which detects tov2's retain behaviour and picks its step
  order from it.
- **Go, in `hack/mto-argocd-migrate`, run with `go run`.** No image, no Helm hook, no new
  direct dependency. Binary releases deferred (Open Questions).
- **`tests/render/engine.go` moves to `internal/gotemplate/`**, with a three-line shim in
  `tests/render` so existing call sites are untouched. One engine, shared by the tests and the
  tool; drift becomes a test failure.
- **Seeding is enforced, derived and refused-on-missing** — the tool writes
  `migration-values.yaml` and will not apply until the chart carries it. It never runs
  `helm upgrade` itself.
- **Order per tenant is fixed in code:** snapshot → derive → predict → diff → disarm purge and
  prove the controller observed it → strip the fossil ownerRef → delete the CR → confirm the
  AppProject survived → one Tenant patch carrying opt-in and overrides together → wait for the
  child → verify against reality.
- **Purge is disarmed with an observation gate**, not a hopeful patch: a spec change plus a
  probe label, then wait for the CR's status timestamp to advance. Same wait proves the
  controller is alive.
- **The fossil `v1beta2` ownerRef is stripped before adoption**, because afterwards `strict`
  forbids the edit — and because GC ignores tov2's retain policy.
- **State is derived from the cluster, never recorded.** No migration CR, no journal file.
  Re-running is the resume path.
- **Preflight is fail-closed on the two fleet-killers:** an unusable
  `privileged.namespaces` list, and an unscoped tov2 sync-enforcement webhook.
- **Snapshots are treated as secrets** (`0700`/`0600`, told to the operator, told to delete
  them) because AppProjects carry `roles[].jwtTokens`.
- **Token loss is refused, not survived.** Adoption destroys `spec.roles[].jwtTokens` and no
  ordering prevents it, so step 0 refuses any tenant that has one, names the roles and token
  ids, and requires `--accept-token-loss` to proceed. Measured 2026-08-23, not reasoned.
- **The plan's claims were tested before approval, not after.** Seven of eight held on
  `kind-mto-argocd-e2e`; the eighth found a pre-existing chart defect. That evidence table
  stays at the top of this document.
- **Deferred, and explicitly out of scope:** the `jwtTokens` apply defect itself — it needs an
  upstream fix (proposed TOV2-10) or an argo-cd CRD change, and this PR only refuses to walk
  into it.
- **Deferred:** binary releases; parallel `--all`; concurrent-run locking. Revisit when a
  customer self-migrates or a cluster exceeds ~50 tenants.

## References

- [TASKS-argocd-extension.md §E](TASKS-argocd-extension.md#e--migration) — the reversals this plan honours (E1: no hook Job; E2: drain per tenant, controller up throughout)
- [live-deployment.md](../live-deployment.md) — the 2026-08-07 `pe` migration and rollback, and "When an AppProject gets deleted"
- [upstream-issues.md](../upstream-issues.md) — TOV2-1 (retain), TOV2-5 (uninstall deadlock), TOV2-9 (unscoped webhook), and "Dual-run collision — measured, not a bug"
- [rollback.md](../rollback.md) — chart-managed rollback, which this plan's rollback is distinct from
- `tenant-operator@main:api/v1alpha1/extensions_types.go` — the CR schema being migrated from
- `tenant-operator@main:internal/controller/extensions/argocd/extensions_argocd_controller.go` — `handleDeletion`, `populateArgoCDSettings`, `handleArgoCDConfiguration`
- `tenant-operator@main:internal/controller/extensions/argocd/extensions_utils.go` — `addRoles`, `removeOldOwnerReference`
- `tenant-operator@main:pkg/builders/appproject.go` — the `make([]T, len(in))` behaviour behind the empty-vs-absent Unknown
- `tenant-operator@main:pkg/utils/namespace/namespace_utils.go:22` — `IsPrivilegedNamespace`, unanchored and fail-open per pattern
- `tenant-operator@main:pkg/config/config.go` — `PrivilegedNamespaces` is the IntegrationConfig's list verbatim, with a `[".*"]` fallback; no hidden defaults, which is why the chart's equivalent filtering is faithful
- `charts/mto-extension-argocd/files/{parent,child}-gotemplate.tpl` and `templates/20-parent-template.yaml` — what the tool reads off the cluster and re-executes
- `tests/render/engine.go` — the renderer mirror the prediction is built on
- `tests/render/equivalence_test.go` — the existing cross-implementation assertion the tool's prediction test extends
