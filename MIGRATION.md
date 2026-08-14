# Migrating from the Extensions controller

Moving a live cluster from `ExtensionsArgoCDReconciler` to this chart, one tenant
at a time.

**The guarantee: no `AppProject` is deleted, and no tenant loses ArgoCD access.**

## The short version

```bash
hack/mto-argocd-migrate --context "$CTX" --operator-namespace <mto-ns> run
```

It works out where you are, shows a checklist, and gives you exactly one next
action. Do it, run it again. Repeat until it says `Done`.

```text
Migration state
  [x] 1  chart installed
  [x] 2  resource lists seeded from the IntegrationConfig
  [ ] 3  per-tenant overrides carried across      <- next
  [ ] 4  onDeletePurgeAppProject false everywhere
  [ ] 5  legacy ownerRefs stripped
  [ ] 6  snapshot taken
  [ ] 7  tenants cut over            0 of 2
  [ ] 8  Extensions CRs drained
```

The split is fixed and deliberate:

| | |
|---|---|
| **You commit** | the chart and its values, `Tenant` annotations, `Extensions` CR edits and deletions — `run` prints the exact YAML |
| **`run --apply` does** | stripping legacy ownerRefs, taking the snapshot, running the diff — cluster-only work with no manifest in Git |
| **You decide** | whether the diff is acceptable. It is shown before the next tenant is offered, never after |

State is read from the cluster every time, never from a file, so `run` is
resumable: interrupt it, re-run it, hand the cluster to a colleague. The only
thing on disk is the snapshot, and its absence is itself a state.

The rest of this document is what `run` is doing and why. Read it before a
production migration; the checklist is not a substitute for knowing why the order
is what it is.

Everything is a dry run until you pass `--apply`.

Evidence for each claim below is marked:

| | |
|---|---|
| **[M]** | measured on a live cluster, 2026-08-07 — see [docs/live-deployment.md](docs/live-deployment.md) |
| **[S]** | read from the controller's source, not executed |
| **[U]** | not verified anywhere. Rehearse before relying on it |

## Contents

- [The short version](#the-short-version)
- [What changes](#what-changes)
- [The order, and why](#the-order-and-why)
- [Before you start](#before-you-start)
- [Scenarios to check for](#scenarios-to-check-for)
- [If your Tenants come from Git](#if-your-tenants-come-from-git)
- [The eight steps](#the-eight-steps)
- [Digging deeper: `inventory`](#digging-deeper-inventory)
- [After the migration](#after-the-migration)
- [Rolling back](#rolling-back)
- [Removing the controller and the CRD](#removing-the-controller-and-the-crd)
- [What this does not migrate](#what-this-does-not-migrate)

## What changes

| | Before | After |
|---|---|---|
| Opting a tenant in | create an `Extensions` CR | annotate the `Tenant` |
| Global ArgoCD settings | `IntegrationConfig.spec.integrations.argocd` | chart values |
| Per-tenant settings | `Extensions.spec.argoCD.appProject` | annotations on the `Tenant` |
| Privileged namespaces/groups | `IntegrationConfig.spec.accessControl.privileged` | **unchanged**, still read live |
| Who writes the `AppProject` | the controller | template-operator-v2 |
| Per-tenant status | `Extensions.status.argocd` | the child `TemplateInstance` |

Nobody gains or loses a capability: MTO grants tenant subjects only `get` on
`tenants/status`, so writing a `Tenant` annotation and writing an `Extensions` CR
are both cluster-admin actions.

### The rendered AppProject

Byte-identical to the controller's — asserted on every test run against captured
controller output — with these deltas:

| Delta | |
|---|---|
| `spec.description` | now `Managed by mto-extension-argocd` **[M]** |
| `spec.roles[].groups` | gains the tenant's **users**, after its groups **[M]** |
| a duplicate ArgoCD-namespace destination | disappears, on tenants that had one **[S]** |

The users are a fix: MTO's generated group only resolves where `syncOCPGroups`
has run, so a tenant whose owners are users had no ArgoCD access at all.

The duplicate arises because the controller appends the ArgoCD namespace without
checking whether the label selector already found it. The chart checks. ArgoCD
reads the two as one.

## The order, and why

Three facts constrain the sequence.

| | Fact | Evidence |
|---|---|---|
| **F1** | The `Extensions` finalizer `tenantoperator.stakater.com/appproject` is released **only** by `ExtensionsArgoCDReconciler` | **[S]**, and a delete completed cleanly with the controller up **[M]** |
| **F2** | Deleting a CR with `onDeletePurgeAppProject: true` destroys the live `AppProject` | **[S]** |
| **F3** | Two writers do not flap — the chart wins, the controller is denied | **[M]** |

**F1** means remove the controller first and every CR sticks in `Terminating`,
and the CRD wedges behind them. Recovery is hand-stripping finalizers.

**F2** means the flag has to be patched to `false` first, as its own write.

**F3**, measured — the `AppProject` never moved, same `resourceVersion`
throughout:

```text
ERROR  Reconciler error  Extensions: <name>
  failed to create or update AppProject: admission webhook
  "sync-enforcement.templates.v2.stakater.com" denied the request: resource is
  managed by sync-enabled TemplateInstance ...
```

Good for the object, bad operationally: the controller enters a permanent
error-retry loop, and it surfaces on the `Extensions` CR where nobody looks.

Together they give one safe order, **per tenant**:

```text
patch purge=false  ->  delete the Extensions CR  ->  annotate the Tenant
      (F2)               (F1: controller must         (F3: by now the controller
                          still be running)            has nothing to reconcile)
```

`cutover` does exactly this, and stops if the `AppProject` does not survive the
middle step.

> **The old controller stays running for the whole migration.** It cannot be
> turned off first (F1), and it does not need to be — a tenant whose CR is gone
> is a tenant it never touches. Removing it is the last step, as part of the
> tenant-operator upgrade. Scaling its `Deployment` to 0 by hand is reverted by
> OLM or Helm on the next resync anyway.

## Before you start

| | |
|---|---|
| Access | cluster-admin |
| Tools | `kubectl`, `jq`, `helm` |
| template-operator-v2 | v0.0.5+, healthy. `sync.mode: strict` needs its webhook, shipped in v0.0.3 |
| tenant-operator | still running, `ExtensionsArgoCDReconciler` still enabled |
| Downtime | none. Steps 1-6 are inert; cutover adopts in place |

Two things to read first:

- [Scenarios to check for](#scenarios-to-check-for) — several are silent if
  missed, and one destroys an `AppProject`
- [If your Tenants come from Git](#if-your-tenants-come-from-git) — if they do,
  do not run any of this imperatively

**Retain is not enforceable yet.** template-operator-v2 deletes every rendered
resource when its `TemplateInstance` goes away ([TOV2-1](docs/upstream-issues.md)),
so `onDeletePurgeAppProject: false` documents intent only. After cutover,
removing a tenant's opt-in annotation **deletes its AppProject** **[M]**. That is
why `scaffolding.mode` defaults to `OnAnnotation`, and why step 6 takes a snapshot.

## Scenarios to check for

Each changes what you have to do. **`inventory` detects the first four and stops
on two of them** — run it before reading further. The last two are questions
about where your manifests live, which no cluster can answer.

### Every `Extensions` CR sets `onDeletePurgeAppProject: true`

Not unusual — it is often applied uniformly. Assume it until `inventory` says
otherwise.

- **"Purge" means delete.** `true` destroys the AppProject when the CR goes
- The retain patch is then **mandatory for every tenant**, not conditional
- Delete the CR without flipping it → the live AppProject is destroyed **[S]**
- Opt the tenant in *first* and it is worse **[S]**: the chart holds the
  AppProject, the controller's delete is denied, `handleDeletion` returns before
  releasing the finalizer, and the CR sticks in `Terminating` forever — so the
  CRD can never be removed

Flip it and confirm it landed **before** anything else touches that tenant.

### A CR sets `sourceRepos`, including `["*"]`

- It must become the `source-repos` annotation, or the rendered AppProject has
  **no** `sourceRepos`
- ArgoCD then refuses every `Application`:
  `application repo ... is not permitted in project '<tenant>'`
- `["*"]` is the worst to miss — maximally permissive today, maximally
  restrictive tomorrow
- Losing a field is not an error, so nothing will tell you. `inventory` flags
  `sourceRepos override` on exactly these tenants

### Tenants are GitOps-managed

`inventory` looks for ArgoCD's tracking metadata — the
`app.kubernetes.io/instance` label or the `argocd.argoproj.io/tracking-id`
annotation — and tells you to stop using `--apply` if it finds any.

- An imperative `kubectl annotate` is drift the instant it is written
- Whether it survives depends on your apply semantics — not a bet worth taking on
  a switch whose removal deletes an `AppProject`
- See [If your Tenants come from Git](#if-your-tenants-come-from-git)

**Absence of that metadata is not an all-clear.** Tracking can be turned off, and
other GitOps tools leave no marker at all, so `inventory` says so explicitly
rather than staying silent. Confirm by hand.

**If the metadata is on an `AppProject`, `inventory` stops the run.** That is a
worse problem than this migration: two writers for an object the chart renders,
and `sync.mode: strict` will start denying those syncs. Take the AppProject
manifests out of Git first — they are generated objects that nobody should be
authoring.

### Some Tenants are `v1beta2` with a `spec.argocd` block

`inventory` reports these. It cannot read the live object — the conversion has
already dropped the block — so it reads
`kubectl.kubernetes.io/last-applied-configuration`, the only place the original
intent survives. A tenant applied by a tool that does not write that annotation
will be missed.

```yaml
apiVersion: tenantoperator.stakater.com/v1beta2
spec:
  argocd:
    sourceRepos: [...]
    appProject:
      clusterResourceWhitelist: [...]
```

- **Nothing reads it, and nothing has for years** — the v1beta2 → v1beta3
  conversion drops it silently
- Do not migrate it, and do not read it as the tenant's current configuration
- If such a tenant has an `AppProject` anyway, find out what created it before
  adopting it

### The `IntegrationConfig` is not in Git

Common — often installed by the operator or set out of band.

- Run `values` against each **live cluster**, not against your repo
- Clusters can differ, so their chart values differ
- Step 2's mismatch check catches a wrong one

### More than one cluster

Each cluster is its own migration: own `values`, own `snapshot`, own `diff`, own
sign-off. A snapshot from one is meaningless against another.

## If your Tenants come from Git

The steps do not change — only who performs the write.

| | Commit | Command |
|---|---|---|
| Chart + values (steps 1-2) | ✓ | |
| Override annotations (step 3) | ✓ | |
| `onDeletePurgeAppProject: false` (step 4) | ✓ | |
| Strip ownerRefs, snapshot, diff (steps 5-6) | | ✓ `run --apply` |
| Delete CRs, opt-in annotation (steps 7-8) | ✓ | |

**`run` writes the commit for you.** It prints the exact YAML for each Git step,
so there is no separate list of commits to keep in sync with this document.

**`strip-owner-refs` stays a command, correctly.** The `AppProject` is
*generated* — nobody authors it, so there is no manifest to edit, and the chart
does not render `ownerReferences` at all. It exists only in cluster state. If you
*do* have `AppProject` manifests in Git, stop: you have two writers for that
object and a bigger problem than this migration.

**One constraint `run` enforces that is easy to get wrong by hand:** step 4 must
land and sync **before** step 7. Deleting a `purge: true` CR destroys the live
AppProject. Steps 7's two changes — deleting the CR and adding the opt-in — can
be one commit, because by then purge is off everywhere.

## The eight steps

`run` names one of these each time. This is what each means, and why the order is
what it is.

**Steps 1-6 touch no tenant's ArgoCD access.** Cutover begins at step 7.

### 1. Chart installed

Run `values` first — it reads only the `IntegrationConfig`, so it works before
the chart exists, and installing with those values saves an upgrade.

```bash
hack/mto-argocd-migrate values > argocd-values.yaml
helm install mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator -f argocd-values.yaml
```

- `scaffolding.mode` defaults to `OnAnnotation`, so this scaffolds nothing
- `argocd.namespace` **must** be where the controller already writes
  `AppProject`s, or the chart creates a second set beside them instead of
  adopting. `run` and `inventory` both refuse to continue on a mismatch

```bash
kubectl get templateinstances.templates.v2.stakater.com mto-argocd-parent \
  -n multi-tenant-operator \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# True
```

Not `Ready` is a chart problem, not a migration problem. It has adopted nothing,
so there is nothing to undo.

### 2. Resource lists seeded

`integrations.argocd.{namespace,clusterResourceWhitelist,namespaceResourceBlacklist}`
are deprecated and the chart no longer reads them. Not carried across → every
adopted `AppProject` silently loses both resource lists.

Only reachable if you skipped `values` at step 1:

```bash
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator -f argocd-values.yaml
```

`accessControl.privileged` is **not** part of this — it stays in the
`IntegrationConfig` and is still read live on every render.

### 3. Per-tenant overrides carried across

Each `Extensions` CR's `sourceRepos`, `clusterResourceWhitelist`,
`namespaceResourceBlacklist` and purge intent become annotations on its `Tenant`.

- An **absent** annotation inherits the chart default
- An explicit `[]` **overrides with empty** — a different thing, so a CR setting a
  list to `[]` is carried across as `[]`, not skipped
- The purge intent is carried **before** step 4 turns the CR's flag off, or the
  tenant's stated intent is lost the day retain becomes enforceable
- Annotations on a tenant that is not opted in do nothing. Still inert

### 4. `onDeletePurgeAppProject` false everywhere

**Its own change, and it must sync before step 7.** "Purge" means delete: a CR
still set to `true` destroys its live `AppProject` the moment it is removed. And
if the chart already held that AppProject, the controller's delete is denied
instead, `handleDeletion` returns before releasing the finalizer, and the CR
sticks in `Terminating` forever — so the CRD can never be removed **[S]**.

### 5. Legacy ownerRefs stripped — `run --apply` does this

- Between tenant-operator v0.3.12 and v0.11.2 the Tenant controller set a
  **controller** ownerRef on the `AppProject` pointing at a `v1beta2` `Tenant`
- A controller ownerRef means the garbage collector deletes the `AppProject` when
  the `Tenant` goes away, overriding any retain setting
- `ExtensionsArgoCDReconciler` stripped it on every reconcile; nothing does once
  that controller is gone
- Only `AppProject`s the controller **never** reconciled still carry one. On most
  clusters there are none
- Stripping one was measured **[M]**. That it removes *only* that ref and keeps
  every other `ownerReference` is pinned by `tests/migrate`, not by the live run —
  the object there carried no other owner

### 6. Snapshot taken — `run --apply` does this

The only record of what the controller produced, and what every later diff
compares against. Kept in `--state-dir` (default `.mto-argocd-migrate`). Keep it
until sign-off; it contains no secrets.

### 7. Tenants cut over

One tenant per `run`, so the diff is read before the next is offered. Each is one
commit: **delete that tenant's `Extensions` CR** and **add the opt-in
annotation**.

`run` then shows the diff against the snapshot. Expect only the deltas in
[What changes](#what-changes), on the tenant you cut over, and **nothing at all**
on any other. A missing `AppProject` is a hard error and exits non-zero.

**This is the go/no-go gate. Read the diff; do not skim it.**

Raise `--batch` once the first tenant is clean. Tenants not yet cut over are
still served by the controller, unchanged, so any pace is fine.

If you drive a cutover with `cutover --apply` instead of a commit, it stops
before doing damage:

| Where | State | Do |
|---|---|---|
| retain patch did not take | CR present, `AppProject` untouched | fix the CR; nothing was deleted |
| `delete` did not complete | CR `Terminating` | the controller is down. **Bring it back up.** Do not clear the finalizer by hand — that skips its retain check |
| `AppProject` gone after the delete | data loss | restore from the snapshot, cut over nobody else until you know why |
| child never `Ready` | `AppProject` **untouched** | a child that fails to render applies nothing. Read the conditions, fix, re-run |

One health check answers for the whole fleet, and prints nothing when all is
well:

```bash
kubectl get templateinstances.templates.v2.stakater.com mto-argocd-parent \
  -n multi-tenant-operator -o json \
  | jq -r '.status.outputs as $o | $o.ready | keys[] as $k
      | select(($o.ready[$k]|fromjson|join("")) != "True")
      | "\($k): \($o.errors[$k]|fromjson|join(" | "))"'
```

### 8. Extensions CRs drained

Zero `Extensions` objects cluster-wide — **including any naming a tenant that
does not exist**. Those produced nothing, but still hold the finalizer that
blocks removing the CRD. `run` lists them by name.

## Digging deeper: `inventory`

`run` shows state; `inventory` shows detail. Read it before a production
migration, and again whenever something is surprising.

| It reports | Meaning |
|---|---|
| `argocd.namespace` mismatch | **hard stop** — the chart would duplicate, not adopt |
| resource lists do not match | step 2 is not done |
| `purge=true, patched to retain at cutover` | step 4 applies to this CR |
| `sourceRepos override` / `whitelist` / `blacklist` | step 3 must carry these across |
| `NO AppProject` | the controller never synced this tenant. Cutover creates one — expect a `NEW` in the diff |
| `orphan AppProject: no Extensions CR` | adopted as-is; nothing to drain |
| `done: chart-managed` | already migrated |
| `no sourceRepos` | the project permits no repositories. True today and it stays true — see below |
| Extensions CRs naming a tenant that does not exist | step 8 |
| legacy `v1beta2` Tenant ownerRefs | step 5 |
| GitOps tracking metadata on Tenants/Extensions | **stop using `--apply`** — every write becomes a commit |
| GitOps tracking metadata on `AppProject`s | **hard stop.** Two writers for a generated object |
| Tenants authored as `v1beta2` with `spec.argocd` | dead config. Do not carry it across |

## After the migration

### Set `sourceRepos` — separately

An `AppProject` with no `sourceRepos` permits **no** repositories, so the tenant
cannot deploy:

```text
InvalidSpecError: application repo https://github.com/... is not permitted in
project 'acme'
```

This is not something the chart introduces. `sourceRepos` was required on the
v1beta2 `Tenant` and became optional on the `Extensions` CR, so any tenant that
never set it already has a project ArgoCD will not deploy from. A faithful
migration reproduces that faithfully.

Fixing it changes what tenants can do, so it is its own change, after the diff is
confirmed. Per tenant, as a `Tenant` annotation:

```yaml
metadata:
  annotations:
    argocd.tenantoperator.stakater.com/source-repos: '["https://github.com/acme/gitops"]'
```

or fleet-wide, as a chart value expanded per tenant:

```yaml
argocd:
  sourceRepos:
    pattern: https://github.com/acme/{{ tenant }}-gitops
```

There is no safe shared *literal* — one org-wide value would let every tenant
deploy from every other tenant's repos.

### Then remove the controller and the CRD

See [Removing the controller and the
CRD](#removing-the-controller-and-the-crd). The order is load-bearing.

## Rolling back

### One tenant, before the CRD is removed

Three ways, in order of preference.

#### 1. Fix forward — usually right

A wrong field came from a chart value or a `Tenant` annotation. Change it and the
`AppProject` is corrected **in place**: no delete, no gap, no lost tokens.

Rollback is for "this chart is wrong for us", not "this field is wrong".

#### 2. Hand it back with no downtime **[U]**

Detach the `AppProject` from the chart *before* removing the opt-in, so the reap
has nothing to reap. The object is never deleted and its UID never changes.

```bash
# 1. strict forbids editing a managed AppProject at all, including by you
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator --set sync.mode=revert --reuse-values

# 2. detach: drop template-operator-v2's finalizer and ownership labels
kubectl patch appproject acme -n <argocd-namespace> --type=merge -p \
  '{"metadata":{"finalizers":null,"labels":{"templates.v2.stakater.com/instance":null,"templates.v2.stakater.com/instance-name":null,"templates.v2.stakater.com/instance-namespace":null,"templates.v2.stakater.com/instance-uid":null}}}'

# 3. the reap now finds nothing it owns, and the AppProject survives
kubectl annotate tenant acme argocd.tenantoperator.stakater.com/enabled-

# 4. hand it to the controller, which adopts the existing object
kubectl apply -f extensions/acme.yaml

# 5. put strict back
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator --set sync.mode=strict --reuse-values
```

The pieces are measured — adoption, the reap, the strict webhook — but **this
sequence is not**. Treat it as an incident procedure, rehearse it on staging, and
`snapshot` first.

#### 3. Delete and recreate — simple, with two real costs

**Order matters** — re-apply the CR *first*.

```bash
# 1. put the Extensions CR back. It error-loops against the strict webhook; that
#    is expected, and being already retrying is what makes step 2 self-heal.
kubectl apply -f extensions/acme.yaml

# 2. remove the opt-in. The chart reaps the child and DELETES the AppProject.
kubectl annotate tenant acme argocd.tenantoperator.stakater.com/enabled-
```

The other order leaves the `AppProject` deleted with nothing running to recreate
it until you apply the CR.

Measured rolling one tenant back **[M]**:

| | |
|---|---|
| `AppProject` absent | **~8 seconds** |
| Content afterwards | byte-identical to the snapshot — `0 changed, 0 new, 0 missing` |
| template-operator-v2 residue | none — no finalizer, label, annotation or ownerRef |
| `metadata.uid` | **changed** |

**A rollback is a delete-and-recreate, not a restore.**

What the gap does, measured by pointing an `Application` at a project that does
not exist **[M]**:

```json
{"sync": "Unknown", "health": "Unknown",
 "conditions": [{"type": "InvalidSpecError",
   "message": "Application referencing project X which does not exist"}]}
```

| | |
|---|---|
| Deployed workloads | **untouched** — `sync: Unknown` means ArgoCD could not compute a diff, and auto-sync acts only on a computed `OutOfSync`, so nothing is pruned |
| `Application` objects | survive. Not owned by the project, not deleted |
| Syncs during the gap | refused, retried once the project returns |
| Project-scoped RBAC | denied for the duration |

What does **not** come back on its own:

- **ArgoCD project role tokens.** `argocd proj role create-token` writes into
  `spec.roles[].jwtTokens`, on the object just destroyed. Any CI pipeline using
  one breaks until reissued. **This is the reason to prefer option 2 for any
  tenant with live tokens**
- Anything holding the old UID — an ownerRef from another object — is dangling

**The risk bigger than the 8 seconds:** this path depends on
`ExtensionsArgoCDReconciler` being up and already retrying. If it is down,
nothing recreates the `AppProject` and the gap lasts until somebody notices.
Confirm it is running *before* removing the annotation.

### The whole release, before the CRD is removed

```bash
# 1. re-apply every Extensions CR you deleted
kubectl apply -f extensions/

# 2. drop the parent. Children and their AppProjects drain while the RBAC still
#    exists -- this DELETES every AppProject the chart manages, and the
#    controller then recreates each one.
kubectl delete templateinstances.templates.v2.stakater.com \
  mto-argocd-parent -n multi-tenant-operator

# 3. wait until no mto-argocd-tenant-* children remain, then
helm uninstall mto-extension-argocd -n multi-tenant-operator

# 4. clear the opt-in markers. kubectl reports "not found" for tenants that
#    never had one; that is noise, not a failure.
kubectl annotate tenant --all argocd.tenantoperator.stakater.com/enabled-
```

Never `helm uninstall` on its own: it deletes the RBAC and the
`TemplateInstance`s together, and each `TemplateInstance`'s finalizer needs that
RBAC to delete its `AppProject`. Teardown deadlocks and every object needs its
finalizer cleared by hand ([TOV2-5](docs/upstream-issues.md)).

Then confirm with `diff ./before-migration` that you are back where you started.

### After the CRD is removed

Not clean. Reverting tenant-operator does not restore a deleted CRD's objects, so
the `Extensions` manifests have to be re-applied from Git. That is why "users
author them in Git" is a load-bearing assumption — and why removing the CRD
waits until the diff has been green for a while.

## Removing the controller and the CRD

A tenant-operator change, not a chart change. Listed here because the **order is
load-bearing**, for the same reason as F1.

1. Confirm zero `Extensions` objects cluster-wide, including any naming a
   non-existent tenant
1. Remove `ExtensionsArgoCDReconciler` and its `Deployment`
   (`extensions-argocd-controller`, gated on `EXTENSIONS_ARGOCD_CONTROLLER` /
   `ENABLE_ALL_CONTROLLERS`)
1. **Then** remove the `Extensions` CRD

Reverse the last two and any surviving CR sticks in `Terminating` forever, and
the CRD wedges behind it.

### Release-note text

Copy into the tenant-operator release that removes this:

> **`Extensions` (ArgoCD) is removed.** Per-tenant ArgoCD `AppProject`s are now
> provided by the `mto-extension-argocd` Helm chart, rendered by
> template-operator-v2.
>
> **Before upgrading**, migrate with the runbook in that chart's `MIGRATION.md`.
> The order is not optional: delete your `Extensions` CRs — with
> `onDeletePurgeAppProject: false`, and **while this version is still running** —
> then upgrade. The finalizer `tenantoperator.stakater.com/appproject` is removed
> only by the controller this release deletes, so a CR that outlives it sticks in
> `Terminating` and blocks the CRD from being removed. Deleting a CR with
> `onDeletePurgeAppProject: true` destroys the live `AppProject`.
>
> `IntegrationConfig.spec.integrations.argocd.{namespace,clusterResourceWhitelist,namespaceResourceBlacklist}`
> are deprecated. The fields are still served and still ignored by this operator;
> their values move into the chart's values.
> `spec.accessControl.privileged` is unaffected and still read live.

## What this does not migrate

| | |
|---|---|
| `Extensions.status.argocd` | replaced by the child `TemplateInstance`'s conditions and the parent's roll-up. Nothing reads the old status |
| The PostgreSQL mirror | `Extensions` is mirrored today; `TemplateInstance` is not, because the database controller watches template-operator **v1** only |
| `Tenant` v1beta2's `spec.argocd.*` | already dead — the conversion drops it silently, and has for years. Not a regression this introduces |
| `sourceRepos` defaults | deliberately empty, so the migration diff stays byte-identical. Step 10 |
| `IntegrationConfig` CRD fields | still served. Removing them is a separate conversion concern, out of scope |
