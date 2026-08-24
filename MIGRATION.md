# Migrating from the Extensions controller

Moving a live cluster from `ExtensionsArgoCDReconciler` to this chart, one tenant
at a time.

## Verdict

- **No `AppProject` is deleted and no tenant loses ArgoCD access.** The chart
  adopts each existing object in place — same UID, no gap. `cutover --apply`
  re-reads the UID after draining the CR and stops if it changed: a same-named
  replacement is not a retained object.
- **One command drives it.** `hack/mto-argocd-migrate ... run` reads the cluster,
  prints a checklist and exactly one next action. Do it, run it again, repeat
  until `Done`. Everything is a dry run until you pass `--apply`.
- **The order is not optional.** Three facts force one sequence per tenant, and
  getting it wrong destroys an `AppProject` or wedges the `Extensions` CRD
  permanently. See [Why the order is what it is](#why-the-order-is-what-it-is).
- **The snapshot is two files per `AppProject`:** a normalised copy for `diff`,
  and `restore/<name>.json` — applyable as-is, ArgoCD project role tokens
  included — for the one path where an object has to be put back.
- **The one dangerous part: retain is not enforceable yet.** template-operator-v2
  deletes every rendered resource when its `TemplateInstance` goes away
  ([TOV2-1](docs/upstream-issues.md)), so after cutover, removing a tenant's
  opt-in annotation **deletes its `AppProject`**. That is why
  `scaffolding.mode` defaults to `OnAnnotation` and why the tool snapshots
  before cutting anyone over.

Everything below was measured on a live cluster on 2026-08-07 —
[docs/live-deployment.md](docs/live-deployment.md) has the transcripts. The two
exceptions are marked **[U]** and should be rehearsed before you rely on them.

## Run it

```bash
hack/mto-argocd-migrate --context "$CTX" --operator-namespace <mto-ns> run
```

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

`run` explains each step as it offers it, so this document does not repeat the
eight steps. It covers what the tool cannot: why the order is what it is, and
what to do when something surprises you.

The split of labour is fixed and deliberate:

| | |
|---|---|
| **You commit** | chart values, `Tenant` annotations, `Extensions` CR edits and deletions — `run` prints the exact YAML |
| **`run --apply` does** | strip legacy ownerRefs, snapshot, diff — cluster-only work with no manifest in Git |
| **You decide** | whether the diff is acceptable. It is shown before the next tenant is offered, never after |

State is read from the cluster every time, never from a file. Interrupt `run`,
re-run it, hand the cluster to a colleague. The only thing on disk is the
snapshot, and its absence is itself a state.

**Steps 1–6 touch no tenant's ArgoCD access.** Cutover begins at step 7.

**If your `Tenant`s come from Git, do not pass `--apply` for steps 3, 4, 7 or 8**
— `run` prints those as YAML to commit, and detects the tracking metadata that
says you are in this case. Steps 5 and 6 stay commands regardless: the
`AppProject` is a generated object with no manifest to edit.

`run` also enforces one constraint that is easy to get wrong by hand: **step 4
must land and sync before step 7.** Step 7's two changes can share a commit,
because by then purge is off everywhere.

### Before you start

| | |
|---|---|
| Access | cluster-admin |
| Tools | `kubectl`, `jq`, `helm` |
| template-operator-v2 | v0.0.5+, healthy. `sync.mode: strict` needs its webhook, shipped in v0.0.3 |
| tenant-operator | still running, `ExtensionsArgoCDReconciler` still enabled |
| Downtime | none |
| Per cluster | each cluster is its own migration: own values, own snapshot, own sign-off. A snapshot from one is meaningless against another |

`run` and `inventory` both hard-stop on the two conditions that make the
migration unsafe — an `argocd.namespace` mismatch, and `AppProject`s managed from
Git — so you cannot walk past either. Run `inventory` as well before your first
`run`, and again whenever something is surprising: it adds the per-tenant detail
`run` does not print.

## Why the order is what it is

| | Fact | If you ignore it |
|---|---|---|
| **F1** | The `Extensions` finalizer `tenantoperator.stakater.com/appproject` is released **only** by `ExtensionsArgoCDReconciler` | Remove the controller first and every CR sticks in `Terminating` with the CRD wedged behind them. Recovery is hand-stripping finalizers |
| **F2** | Deleting a CR with `onDeletePurgeAppProject: true` destroys the live `AppProject` | Data loss. So the flag is patched to `false` first, as its own write |
| **F3** | Two writers do not flap — the chart wins, the controller is denied by the `sync-enforcement` webhook | The `AppProject` is safe (same `resourceVersion` throughout), but the controller enters a permanent error-retry loop, surfacing on the `Extensions` CR where nobody looks |

Together they give one safe order, **per tenant**:

```text
patch purge=false  ->  delete the Extensions CR  ->  annotate the Tenant
      (F2)               (F1: controller must         (F3: by now the controller
                          still be running)            has nothing to reconcile)
```

> **The old controller stays running for the whole migration.** It cannot be
> turned off first (F1), and it does not need to be — a tenant whose CR is gone
> is a tenant it never touches. Removing it is the last step, part of the
> tenant-operator upgrade. Scaling its `Deployment` to 0 by hand is reverted by
> OLM or Helm on the next resync anyway.

## What changes

| | Before | After |
|---|---|---|
| Opting a tenant in | create an `Extensions` CR | annotate the `Tenant` |
| Global ArgoCD settings | `IntegrationConfig.spec.integrations.argocd` | chart values |
| Per-tenant settings | `Extensions.spec.argoCD.appProject` | annotations on the `Tenant` |
| Privileged namespaces/groups | `IntegrationConfig.spec.accessControl.privileged` | **unchanged**, still read live on every render |
| Who writes the `AppProject` | the controller | template-operator-v2 |
| Per-tenant status | `Extensions.status.argocd` | the child `TemplateInstance` |

Nobody gains or loses a capability: MTO grants tenant subjects only `get` on
`tenants/status`, so writing a `Tenant` annotation and writing an `Extensions` CR
are both cluster-admin actions.

The rendered `AppProject` is **byte-identical** to the controller's — asserted on
every test run against captured controller output — with three deltas:

| Delta | Why |
|---|---|
| `spec.description` is now `Managed by mto-extension-argocd` | cosmetic |
| `spec.roles[].groups` gains the tenant's **users**, after its groups | a fix. MTO's generated group only resolves where `syncOCPGroups` has run, so a tenant whose owners are users had no ArgoCD access at all. The generated group is still emitted, so nothing is lost where it does exist |
| a duplicate ArgoCD-namespace destination disappears | the controller appends the namespace without checking whether the label selector already found it. ArgoCD read the two as one |

## What can bite you

`inventory` detects everything above the line and hard-stops on two. The rest are
questions about where your manifests live, which no cluster can answer.

| Condition | Why it matters | `inventory` |
|---|---|---|
| `argocd.namespace` differs from where the controller writes | the chart would create a **second** set of `AppProject`s beside the live ones instead of adopting them | **hard stop** |
| GitOps tracking metadata on an `AppProject` | two writers for a generated object, and `sync.mode: strict` will start denying those syncs. Take the manifests out of Git first — nobody should be authoring these | **hard stop** |
| Every CR sets `onDeletePurgeAppProject: true` | common, often applied uniformly. The retain patch becomes **mandatory for every tenant**, not conditional. Opting the tenant in first is worse than deleting without the patch: the controller's delete is denied, `handleDeletion` returns before releasing the finalizer, and the CR sticks in `Terminating` forever | flags each CR |
| A CR sets `sourceRepos`, especially `["*"]` | it must become the `source-repos` annotation or the rendered `AppProject` has **no** `sourceRepos` and ArgoCD refuses every `Application`. Losing a field is not an error, so nothing else will tell you. `["*"]` is the worst to miss — maximally permissive today, maximally restrictive tomorrow | flags `sourceRepos override` |
| Resource lists not seeded into chart values | `integrations.argocd.{namespace,clusterResourceWhitelist,namespaceResourceBlacklist}` are deprecated and the chart no longer reads them. Every adopted `AppProject` silently loses both lists | reports the mismatch |
| Legacy `v1beta2` `Tenant` ownerRefs | between tenant-operator v0.3.12 and v0.11.2 the Tenant controller set a **controller** ownerRef on the `AppProject`, so the GC deletes it when the `Tenant` goes away — overriding any retain setting. The old controller stripped it on every reconcile; nothing will once it is gone. Only `AppProject`s it never reconciled still carry one | flags them; `run --apply` strips them |
| Tenants authored as `v1beta2` with a `spec.argocd` block | dead config. The v1beta2 → v1beta3 conversion has dropped it silently for years. **Do not carry it across, and do not read it as current configuration.** If such a tenant has an `AppProject` anyway, find out what created it before adopting | reads `last-applied-configuration`, the only place the intent survives — a tenant applied by a tool that does not write that annotation is missed |
| No `AppProject` for a tenant | the controller never synced it. Cutover creates one — expect a `NEW` in the diff | reports it |
| An `AppProject` with no `Extensions` CR | adopted as-is; nothing to drain | reports it |
| — | | |
| `Tenant`s are GitOps-managed | an imperative `kubectl annotate` is drift the instant it is written. Not a bet worth taking on a switch whose removal deletes an `AppProject` | detects ArgoCD's tracking metadata (the `app.kubernetes.io/instance` label or the `argocd.argoproj.io/tracking-id` annotation) and tells you to stop using `--apply`. **Absence is not an all-clear** — tracking can be off and other tools leave no marker, so it says so explicitly rather than staying silent |
| The `IntegrationConfig` is not in Git | common. Run `values` against each **live cluster**, not against your repo — clusters differ, so their chart values differ | step 2 compares the chart's resource lists against the live `IntegrationConfig` by content, and refuses to advance on a difference |

## The go/no-go gate

Step 7 cuts over one tenant per `run`, then shows the diff against the snapshot.
**Read it; do not skim it.** Expect only the three deltas above, on the tenant you
cut over, and **nothing at all** on any other. A missing `AppProject` is a hard
error and exits non-zero.

Raise `--batch` once the first tenant is clean. Tenants not yet cut over are
still served by the controller, unchanged, so any pace is fine.

If you drive a cutover with `cutover --apply` instead of a commit, it stops before
doing damage:

| State | Meaning | Do |
|---|---|---|
| CR present, `AppProject` untouched | the retain patch did not take | fix the CR; nothing was deleted |
| CR `Terminating` | the controller is down | **bring it back up.** Do not clear the finalizer by hand — that skips its retain check |
| `AppProject` gone after the delete | data loss | `kubectl apply -f <snapshot>/restore/<tenant>.json`, then cut over nobody else until you know why |
| `AppProject` present, but a **new UID** | it was deleted and recreated, not retained. Its role tokens are gone and any ownerRef naming the old UID now dangles | stop. Find out what deleted it before cutting over anyone else |
| child never `Ready`, `AppProject` untouched | a child that fails to render applies nothing | read the conditions, fix, re-run |

One check answers for the whole fleet, and prints nothing when all is well:

```bash
kubectl get templateinstances.templates.v2.stakater.com mto-argocd-parent \
  -n multi-tenant-operator -o json \
  | jq -r '.status.outputs as $o | $o.ready | keys[] as $k
      | select(($o.ready[$k]|fromjson|join("")) != "True")
      | "\($k): \($o.errors[$k]|fromjson|join(" | "))"'
```

## After the migration: set `sourceRepos`

An `AppProject` with no `sourceRepos` permits **no** repositories, so the tenant
cannot deploy:

```text
InvalidSpecError: application repo https://github.com/... is not permitted in
project 'acme'
```

**The chart does not introduce this.** `sourceRepos` was required on the v1beta2
`Tenant` and became optional on the `Extensions` CR, so any tenant that never set
it already has a project ArgoCD will not deploy from. A faithful migration
reproduces that faithfully — which is why the chart's default is deliberately
empty and the diff stays byte-identical.

Fixing it changes what tenants can do, so it is **its own change, after the diff
is confirmed.** Per tenant:

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

## Rolling back

Three options while the CRD still exists. Commands are in
[docs/rollback.md](docs/rollback.md).

| | When | Cost |
|---|---|---|
| **1. Fix forward** | a wrong field came from a chart value or an annotation. Change it and the `AppProject` is corrected **in place** | none. No delete, no gap, no lost tokens. Rollback is for "this chart is wrong for us", not "this field is wrong" |
| **2. Hand it back** | a tenant with live ArgoCD project role tokens | none measured, but the **sequence is unverified [U]**. Detach the object from the chart *before* removing the opt-in, so the reap has nothing to reap. Treat as an incident procedure: rehearse on staging, snapshot first |
| **3. Delete and recreate** | anything else | an ~8-second gap, a new UID, and **lost project role tokens** |

Measured for option 3, one tenant:

| | |
|---|---|
| `AppProject` absent | **~8 seconds** |
| Content afterwards | byte-identical to the snapshot — `0 changed, 0 new, 0 missing` |
| template-operator-v2 residue | none — no finalizer, label, annotation or ownerRef |
| `metadata.uid` | **changed** |

**A rollback is a delete-and-recreate, not a restore.** During the gap, an
`Application` pointing at the missing project reports `sync: Unknown` /
`health: Unknown` with an `InvalidSpecError`, but:

- **deployed workloads are untouched** — `sync: Unknown` means ArgoCD could not
  compute a diff, and auto-sync acts only on a computed `OutOfSync`, so nothing
  is pruned
- `Application` objects survive; they are not owned by the project
- syncs are refused during the gap and retried once the project returns
- project-scoped RBAC is denied for the duration

What does **not** come back on its own:

- **ArgoCD project role tokens.** `argocd proj role create-token` writes into
  `spec.roles[].jwtTokens` on the object just destroyed. Any CI pipeline using
  one breaks until reissued. **This is the reason to prefer option 2 for any
  tenant with live tokens.**
- anything holding the old UID — an ownerRef from another object — is dangling

**The risk bigger than the 8 seconds:** option 3 depends on
`ExtensionsArgoCDReconciler` being up and already retrying. If it is down,
nothing recreates the `AppProject` and the gap lasts until somebody notices.
Confirm it is running *before* removing the annotation.

**Once the CRD is removed, rollback is not clean.** Reverting tenant-operator
does not restore a deleted CRD's objects, so the `Extensions` manifests have to
be re-applied from Git. That is why "users author them in Git" is a load-bearing
assumption, and why removing the CRD waits until the diff has been green for a
while.

## Removing the controller and the CRD

A tenant-operator change, not a chart change. Listed here because the **order is
load-bearing**, for the same reason as F1.

1. Confirm zero `Extensions` objects cluster-wide — **including any naming a
   tenant that does not exist.** Those produced nothing but still hold the
   finalizer that blocks removing the CRD. `run` lists them by name
1. Remove `ExtensionsArgoCDReconciler` and its `Deployment`
   (`extensions-argocd-controller`, gated on `EXTENSIONS_ARGOCD_CONTROLLER` /
   `ENABLE_ALL_CONTROLLERS`)
1. **Then** remove the `Extensions` CRD

Reverse the last two and any surviving CR sticks in `Terminating` forever, with
the CRD wedged behind it.

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
| `sourceRepos` defaults | deliberately empty, so the migration diff stays byte-identical. See [above](#after-the-migration-set-sourcerepos) |
| `IntegrationConfig` CRD fields | still served. Removing them is a separate conversion concern, out of scope |
