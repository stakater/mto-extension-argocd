# Upstream issues found while building this chart

Drafts ready to file against `template-operator-v2`. Every one was reproduced
first-hand; the reproduction steps are exact.

Two environments produced them, and each entry says which:

- the live OpenShift 4.20 cluster (MTO v1.8.2, tov2 **v0.0.5**, ArgoCD 3.4.6) —
  TOV2-1 through TOV2-8;
- the kind E2E cluster (Kubernetes 1.33.4, ArgoCD 3.4.6), run against both the
  published tov2 **v0.0.6** and a build of `8a44194` — TOV2-9, plus the status
  updates on TOV2-1 and TOV2-8 that the newer build made visible.

**The version gap matters when reading this.** Behaviour fixed in `8a44194` is
still broken on the v0.0.5 the live cluster runs, and the E2E cluster is
currently the only place the newer build has been exercised against this chart.

---

## TOV2-1 — retain on delete

**Status: FIXED UPSTREAM, unreleased.** Verified 2026-08-18 against
template-operator-v2 at `8a44194` ("Honour a per-resource deletion policy when
releasing rendered resources") on the kind E2E cluster: descaffolding a tenant
now **releases** its AppProject instead of deleting it, and the released object
comes back with no finalizer, no ownership labels and only this chart's own
annotation on it — so it is immediately deletable by a human. The annotation key
this chart guessed, `templates.v2.stakater.com/deletion-policy: Orphan`, is the
one the operator implemented.

`tests/e2e` asserts both directions now: retain is reported (it depends on the
operator build underneath), and purge — `on-delete-purge: "true"` — is asserted
outright, because that direction holds on every build.

**No released build has it yet.** Measured against the current published
release, v0.0.6, on 2026-08-18: descaffolding still deleted the AppProject. The
live cluster runs v0.0.5. Everything below describes that behaviour and applies
until the fix ships.

Originally: known, unfiled. Blocks `C10` and the Phase 2 rollout.

`handleDeletion` deletes every rendered resource unconditionally, and
`onDeletePurgeAppProject` has no kubebuilder default — so unset means false,
which makes **retain the common path**. There is no way today to say "stop
managing this, but do not delete it".

Demonstrated: flipping one chart value from `scaffolding.mode: Always` to
`OnAnnotation` deleted two tenants' AppProjects. Reproduced again on
2026-08-06: removing the opt-in annotation from tenant `sos` reaped its child
TemplateInstance and its AppProject.

**The trap, which must be spelled out when filing.** Implement it as a
**delete-time policy check**, not by withholding ownership labels. Ownership
labels drive updates and orphan tracking too, so withholding them freezes a
retained AppProject at creation-time content forever — and since retain is the
default, that breaks every tenant while reporting success.

### Acceptance criteria

- a retained resource keeps receiving updates
- it carries no dangling finalizer (see TOV2-2 below — these interact)
- the parent cascade honours the policy

This chart already stamps
`templates.v2.stakater.com/deletion-policy: Orphan` on every AppProject it
renders with purge disabled. That key is a **guess**: the operator has no
deletion-policy surface at all today, so the annotation is currently inert.
Agree the real key when filing, and the chart follows it.

---

## TOV2-2 — under `sync.mode: revert`, deleting a rendered resource wedges it in Terminating

> Two distinct defects were originally filed under this number and are now
> split: **TOV2-2** is the wedge itself (the finalizer is never released), and
> **TOV2-8** below is its cause (the drift check does not look at
> `deletionTimestamp`). They want separate fixes -- releasing the finalizer
> stops the object being stuck, but only the drift check makes the resource
> come back.

**Status:** new, found 2026-08-06. Not yet filed.

### Severity: medium — there is a supported setting that prevents it

`sync.mode: strict` adds an admission webhook that refuses the delete outright,
so the wedge never happens. That is why this chart now defaults to `strict`.
The defect below is what happens under `revert`, which is what most people will
reach for first because it sounds like the safe middle option.

Verified on the same cluster, tov2 v0.0.5:

```text
$ kubectl delete appproject sos -n stakater-tenant-operator
Error from server (Forbidden): admission webhook "sync-enforcement.templates.v2.stakater.com"
denied the request: resource is managed by sync-enabled TemplateInstance
stakater-tenant-operator/mto-argocd-sos; deletion is not allowed
```

Teardown is not blocked by the webhook: it allows the delete once the owning
TemplateInstance is itself being deleted
(`rendered_resource_webhook.go:109-113`), so descaffolding a tenant and
`helm uninstall` are both permitted. They still have to be done in the right
ORDER, for an unrelated reason -- see TOV2-5.

The residual bug is that `revert` has a destructive, silent failure mode that
`strict` merely sidesteps.

### Reproduce

```bash
# with a TemplateInstance rendering AppProject/<tenant>, Ready=True
kubectl delete appproject <tenant> -n <argo-namespace>
```

### Observed

The AppProject never goes away and never comes back:

```text
deletionTimestamp: "2026-08-06T13:08:08Z"
finalizers: ["templates.v2.stakater.com/template-instance-resource"]
```

Still wedged at 163s, and indefinitely thereafter. Meanwhile the operator logs

```text
DEBUG  No drift detected, skipping apply  ... "kind": "AppProject", "name": "sos"
INFO   Successfully reconciled TemplateInstance ...
```

and the owning TemplateInstance reports every condition True:

```text
TemplateResolved=True  ParametersValid=True  Rendered=True
Synced=True  Applied=True  Ready=True  OutputsResolved=True
```

### Why it matters

ArgoCD will not use an AppProject that is `Terminating`, so the tenant loses
ArgoCD access — while every piece of status says the system is healthy. "Delete
it and let the operator rebuild it" is a reflex action for anyone operating a
reconciler, and here it is destructive and undetectable from status.

### Root cause, as far as we can see from outside

The drift check compares the rendered spec against the live object. A deleted
object still *has* its spec until the finalizer clears, so drift is `false` and
apply is skipped. Nothing in that path notices `metadata.deletionTimestamp`, so
the finalizer is never released and the resource is never re-created.

### Suggested fix

Release the finalizer once the owning TemplateInstance no longer wants the
resource deleted, so the object stops being stuck. Recreating it afterwards is
TOV2-8. At minimum, surface it: a TemplateInstance whose owned resource is
terminating must not report `Ready=True`.

### Workaround

```bash
kubectl patch appproject <tenant> -n <argo-namespace> \
  --type=merge -p '{"metadata":{"finalizers":[]}}'
```

The operator then rebuilds the AppProject correctly (verified).

Pinned by `tests/e2e` as a characterization test that installs the chart with
`sync.mode: revert` on purpose — it asserts the **bug**, so it will start
failing when this is fixed, which is the signal to update it. A sibling test
asserts that the shipped default, `strict`, denies the delete.

---

## Why roles gain the tenant's users — proof, not reasoning

The chart adds the tenant's `users` to `roles[].groups` alongside the generated
group. The justification was always "the generated group only resolves on
OpenShift". On 2026-08-06 this turned out to be observable on the live cluster,
and worse than the argument assumed.

`AppProject/pe`, as produced by the controller today, has:

```yaml
roles:
  - name: pe-owner
    groups: [pe-owner-group-489fab]
```

That group **does not exist**:

```bash
$ kubectl get groups.user.openshift.io
org-c8iu77i5a1p4d-realm-default-group
stakater-admin
stakater-reader
```

`Tenant/pe` defines its owners as a **user** (`ayesha.arshad@stakater.com`) with
no groups. So the only entry in `pe-owner.groups` names a group nobody is in.
**`AppProject/pe` currently grants ArgoCD access to nobody.**

### Why the group is missing

`syncOCPGroups` -- which creates `<tenant>-<role>-group-<hash>` from
`AccessControl.Owners.Users` -- sits inside
`case constants.PlatformTypeOpenshift` (`tenant_create_update.go:73-84`). The
platform is not detected; it is read from an environment variable
(`pkg/config/config.go:475`). On this cluster:

```bash
$ kubectl get deploy tenant-operator-tenant-controller -o jsonpath='{...env}'
PLATFORM_TYPE=Kubernetes
```

on an OpenShift 4.20 cluster. So the branch never runs, no groups are created,
and every AppProject references groups that will never exist.

`Tenant/pe` still reports `Ready=True, ReconcileSuccessful` throughout -- there
is no `OCPSyncError`, because the code never got far enough to fail.

### A related MTO defect, worth filing with A3

Every call site discards the error:

```go
platformType, _ := config.GetPlatformType()
```

`GetPlatformType` returns an error for any value that is not exactly
`Openshift` or `Kubernetes`, including unset. With the error dropped,
`platformType` is `""`, every OpenShift-only branch is skipped silently, and
the Tenant still reports success. A typo in that variable degrades an OpenShift
cluster to a Kubernetes one with no signal at all.

### What this means for the chart

The users fix is not defensive, it is corrective: without it the migration would
faithfully reproduce AppProjects that grant access to nobody. Keeping the
generated group as well costs nothing -- it becomes meaningful the moment
`PLATFORM_TYPE` is set correctly and MTO creates the groups.

---

## Dual-run collision — measured, not a bug

Not an upstream issue. Recorded here because it settles how Phase 2 has to be
sequenced, and it was measured on the live cluster on 2026-08-06 rather than
reasoned about.

**Question.** During a dual-run, both `ExtensionsArgoCDReconciler` and this
chart can write `AppProject/<tenant>`. Who wins, and what breaks?

**Method.** Tenant `pe` — which has a live `Extensions/pe-extensions` CR and a
controller-owned `AppProject/pe` — was opted in to the chart. The controller's
pod was then deleted, because controller-runtime does a full resync of every
Extensions CR on startup, which is exactly the nudge a production deploy, node
drain or upgrade gives it.

**Result.** The chart wins outright, and the controller is locked out by the
`strict` sync webhook:

```text
ERROR  Reconciler error  Extensions: pe-extensions
  error: failed to create or update AppProject: admission webhook
  "sync-enforcement.templates.v2.stakater.com" denied the request: resource is
  managed by sync-enabled TemplateInstance stakater-tenant-operator/mto-argocd-pe;
  modifications to enforced fields are not allowed
```

`AppProject/pe` did not move -- same resourceVersion throughout.

**What this means for Phase 2.**

- There is **no silent flapping**. Two writers do not alternate and leave the
  object oscillating; the chart holds it and the controller is refused. That is
  the good outcome, and it is `strict` that produces it -- under `revert` the
  two would take turns.
- But the controller does not fail quietly. It enters a permanent
  error-retry loop, and the failure surfaces on the `Extensions` CR rather than
  anywhere an operator is looking.
- So **the controller must be off before any tenant is opted in**, which is
  what the plan already specifies: default-off and out of the
  `ENABLE_ALL_CONTROLLERS` branch. This measurement is the reason, not a
  precaution.

**One useful property.** The collision is self-healing in the reverting
direction. Removing the opt-in annotation reaped the child, deleted the
AppProject, and the controller -- already retrying -- recreated it within
seconds. Verified byte-for-byte against a snapshot taken beforehand: identical
apart from destination ordering, which comes from an API-server LIST and
carries no meaning. No tov2 finalizer, annotation or label was left behind.

That self-healing only holds **because** the controller was already in a retry
loop. A controller that had gone quiet again might not notice for a long time,
so a rollback plan should not depend on it.

---

## TOV2-9 — the chart CHECKED INTO the repository ships the sync-enforcement webhook unscoped, and it deadlocks the cluster it is installed into

**Status:** new, found 2026-08-18 while standing up the kind E2E cluster. Not
yet filed.

**Scope, measured both ways — this is the first thing to say when filing:**

| Chart | `matchConditions` | Consequence |
|---|---|---|
| published `oci://ghcr.io/stakater/public/charts/template-operator-v2` **0.0.6** | **present** — pulled and inspected 2026-08-18 | fine. No customer is exposed today |
| `charts/template-operator-v2/` checked in at **`8a44194`** | **absent** | installing it deadlocks the whole cluster, unrecoverably |

So this is not a live customer incident; it is an unenforced guarantee, and the
unlucky path is the one a developer takes.

### What

The webhook matches `UPDATE` and `DELETE` on `*/*/*` with `failurePolicy: Fail`.
`config/webhook/manifests.yaml` narrows that with a `matchConditions` CEL filter
on the `templates.v2.stakater.com/instance` label. **helmify drops
`.webhooks[].matchConditions`**, so a postprocess step re-injects it at release
time and a helm unittest guards that step. The release pipeline runs both; the
chart *committed to the repository* is the pre-postprocess output, so
`helm install ./charts/template-operator-v2` from a checkout installs the
unscoped webhook.

### Why it is not merely "too broad"

It is a circular deadlock, and every step was observed on a fresh kind cluster
(Kubernetes 1.33.4):

1. Helm creates the ValidatingWebhookConfiguration before the operator's pod is
   ready, so every `UPDATE` in the cluster fails closed:

   ```text
   E0818 09:08:47 leaderelection.go:472] Failed to update lock: Internal error occurred:
     failed calling webhook "sync-enforcement.templates.v2.stakater.com": ...
     dial tcp 10.96.1.228:443: connect: connection refused
   ```

   That is **kube-controller-manager**, unable to renew its leader-election
   Lease.

2. A controller-manager without leadership does not run the EndpointSlice
   controller, so the webhook's own Service never receives an endpoint.

3. The webhook is therefore unreachable for ever. `helm install --wait` failed
   with `context deadline exceeded`; the operator's own Deployment reported
   `Available=False MinimumReplicasUnavailable` while its pod was `1/1 Running`,
   because no controller was left to update the ReplicaSet's status.

Nothing self-heals: the component that would create the endpoint is the one
being blocked by the missing endpoint.

### Recovery, and why it works

Patching the webhook is possible even from inside the deadlock -- the API server
does not consult admission webhooks for requests on `admissionregistration.k8s.io`
objects. Adding the `matchConditions` block from `config/webhook/manifests.yaml`
restored the cluster in under a second: leases renewed, endpoints appeared, the
Deployment went `Available` immediately.

[`hack/e2e/scope-tov2-webhook.sh`](../hack/e2e/scope-tov2-webhook.sh) does this
automatically before the E2E suite waits on anything, and prints a warning when
it has to.

### Why this chart cares

`values.yaml` originally told operators that `sync.mode: strict` has a bounded
blast radius: "the webhook carries a matchConditions CEL filter ... so the API
server never consults it for anything else." True of the published chart, false
of the checked-in one, and the difference is not degraded drift protection but
a dead cluster. That wording is gone: `values.yaml` now says the blast radius
holds "ONLY IF that webhook carries its matchConditions CEL filter", and
carries the one-line check to run rather than the bare claim.

### Suggested fix

Stop depending on a postprocess step to re-add a field that is load-bearing for
availability, and stop committing the pre-postprocess chart. Either template the
webhook by hand with `matchConditions` inline, or make the committed chart the
postprocessed one and fail the build when it lacks the filter — the existing
helm unittest only helps if someone runs it. The property to guarantee is:
**the webhook cannot be installed without its filter, from any copy of the
chart.**

### Acceptance criteria

- `helm install` from any published or local build of the chart yields a webhook
  with `matchConditions` present
- killing the operator pod does not affect any object without the
  `templates.v2.stakater.com/instance` label -- specifically, leases in
  `kube-system` keep renewing

---

## TOV2-8 — drift detection ignores deletionTimestamp

**Status:** new, found 2026-08-06. Not yet filed. The cause behind TOV2-2, split
out because it needs its own fix.

`detectDrift` GETs the live object and compares the rendered spec against it. A
resource that is being deleted still HAS its spec until the finalizer clears, so
the comparison finds no difference and apply is skipped:

```text
DEBUG  No drift detected, skipping apply  ... "kind": "AppProject", "name": "sos"
```

Nothing in that path examines `metadata.deletionTimestamp`. So even once the
finalizer is released (TOV2-2), the operator has no reason to recreate what was
deleted -- it believes the object matches.

### Suggested fix

Treat a non-nil `deletionTimestamp` on an owned resource as drift. That makes
the delete complete and the resource be re-applied from the template, which is
what `revert` promises.

### A second symptom, same cause — measured 2026-08-18 on the kind E2E cluster

A resource deleted **out of band while unowned**, whose tenant is then opted
back in, is not recreated either. The child TemplateInstance reconciles every 30
seconds and logs the same line each time:

```text
DEBUG  No drift detected, skipping apply  ... "kind": "AppProject", "name": "e2e-alpha"
```

for over two minutes, while the AppProject genuinely does not exist. The
comparison is against a resource the operator's informer cache still holds, so
"no drift" is measured against something that is gone. Reconciling on a timer
does not help, because the cache is what is stale, not the trigger.

The E2E suite works around it by re-adopting rather than deleting and waiting
for a fresh create — see the purge subtest in `tests/e2e/e2e_test.go`. Worth
naming when filing: the fix for the deletionTimestamp case does not cover this
one, because here there is no object to carry a deletionTimestamp at all.

---

## TOV2-7 — TemplateInstance has no printer columns

**Status:** new, found 2026-08-06. Not yet filed. Small, and it improves every
consumer of the operator at once.

`kubectl get templateinstances.templates.v2.stakater.com -n <ns>` prints only
NAME and AGE. For a parent that fans out one child per tenant, that list is the
natural per-tenant view -- one object each, already tabular -- and it shows
nothing about whether any of them is healthy.

Every answer has to be spelled out by hand:

```bash
kubectl get templateinstances.templates.v2.stakater.com -n <ns> \
  -o custom-columns='NAME:.metadata.name,\
READY:.status.conditions[?(@.type=="Ready")].status,\
REASON:.status.conditions[?(@.type=="Ready")].reason'
```

### Suggested fix

Three markers on `TemplateInstance`:

```go
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

Worth considering `Template` too. Note this is the same gap that makes the
parent's aggregate output feel necessary in the first place: with columns on
the children, `kubectl get` answers "how is each tenant doing" directly, and
the parent's roll-up is only needed for "is anything broken at all".

For comparison, `mto-extension-openbao` does define printer columns -- but on
its own CRDs (`OpenBaoPKISecretEngineConfig` and friends), which it owns. A
chart-only extension has no CRD of its own, so it cannot do the same and
depends on the operator's.

---

## TOV2-6 — status outputs cannot be grouped by source object

**Status:** new, found 2026-08-06. Not yet filed. A usability request, not a
defect.

`TemplateInstanceStatus.Outputs` is `map[string]map[string]string` — field
name, then keyTemplate key. For a parent aggregating N children with `fromAll`,
that produces one map per field, each holding one entry per child:

```yaml
outputs:
  ready:  {mto-argocd-alpha: '["True"]',  mto-argocd-beta: '["False"]'}
  reason: {mto-argocd-alpha: '["Reconciled"]', mto-argocd-beta: '["TemplateExecutionFailed"]'}
  errors: {mto-argocd-alpha: '[]', mto-argocd-beta: '["failed to execute template: ..."]'}
```

Reading one child's state means correlating across every map by key, which is
fine at one child and unusable at fifty. The shape people want is source-first:

```yaml
outputs:
  mto-argocd-alpha: {ready: "True", reason: Reconciled, errors: []}
  mto-argocd-beta:  {ready: "False", reason: TemplateExecutionFailed, errors: [...]}
```

### Suggested fix

An opt-in on the output, defaulting to today's shape so nothing breaks:

```yaml
outputs:
  - name: tenants
    fromAll: {...}
    groupBy: key      # key (source-first) | field (current default)
```

This needs the status type to gain a level
(`map[string]map[string]map[string]string`) or a separate field, so it is an
API change either way — hence a request rather than a bug.

Workaround in the meantime is a jq reduce, which is fine for a human at a
terminal and unhelpful for anything consuming the status programmatically.

---

## TOV2-5 — `helm uninstall` deadlocks its own teardown

**Status:** new, found 2026-08-06. Not yet filed.

### Severity: high for anyone who uninstalls

It leaves every rendered object wedged, and recovery is hand-clearing
finalizers one object at a time.

`helm uninstall` issues all deletes at once. The RBAC objects carry no
finalizers, so they are gone immediately. Every `TemplateInstance` lingers on
`templates.v2.stakater.com/template-instance`, and that finalizer needs to
delete the AppProjects it owns — using the RBAC that has just been removed.
Nothing can complete.

Observed after an E2E run, long after the uninstall returned "success". The
suite had removed the opt-in annotation first, but did not WAIT for the reap to
finish before uninstalling -- which is exactly the mistake a human makes too:

```text
TemplateInstance/mto-argocd-sos  deletionTimestamp=2026-08-06T13:31:43Z
                                 finalizers=[templates.v2.stakater.com/template-instance]
Template/mto-argocd-child        deletionTimestamp=2026-08-06T13:31:43Z
                                 finalizers=[templates.v2.stakater.com/finalizer]
AppProject/sos                   still present, orphaned
```

Recovery is manual, on every object:

```bash
kubectl patch <kind>/<name> -n <ns> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

### Relationship to the existing RBAC rule

The chart already grants the reap verbs unconditionally and gates only
`create`, because gating the whole block behind the feature toggle deadlocks
finalization. This is the same failure on a different axis — the RBAC is not
gated off, it is *deleted* — and no chart-side rule can prevent it, because
Helm decides the deletion order and does not wait for finalizers.

### Workaround, documented in NOTES.txt

Delete the parent TemplateInstance first, let the children and their
AppProjects drain while the RBAC still exists, and only then `helm uninstall`.

### Suggested fix

Either the TemplateInstance finalizer should give up and release after a
bounded number of permission failures rather than retrying forever, or it
should surface the RBAC failure on the TemplateInstance status instead of
silently looping. Today the object simply sits there with no indication of why.

---

## TOV2-4 — `helm uninstall` returns before the Template's finalizer is released

**Status:** new, found 2026-08-06. Not yet filed. Low severity, but it bites
during exactly the operation people are least patient with.

`helm uninstall` completes as soon as the API server accepts the deletes, but
template-operator-v2 holds a finalizer on its own `Template`, so
`Template/mto-argocd-parent` outlives the command by several seconds.
Reinstalling in that window fails:

```text
Error: INSTALLATION FAILED: Unable to continue with install: Template
"mto-argocd-parent" in namespace "stakater-tenant-operator" exists and cannot
be imported into the current release: invalid ownership metadata; annotation
validation error: key "meta.helm.sh/release-name" must equal "<new>": current
value is "<old>"
```

Observed both by hand and in the E2E suite, where the second test's install
failed against the first test's not-yet-collected Template.

The correct response is to wait, not to force ownership or delete the object —
it is being finalized normally. The E2E harness now blocks until the Template
is actually gone, and any uninstall/reinstall runbook should do the same.

---

## TOV2-3 — the CRDs do not install on Kubernetes 1.31

**Status:** new, found 2026-08-06. Not yet filed. Low severity, easy to hit.

Installing `templates.v2.stakater.com_templateinstances.yaml`
(v0.0.6-3-g5fd8f5e) on a 1.31 API server fails:

```text
CustomResourceDefinition.apiextensions.k8s.io "templateinstances.templates.v2.stakater.com"
is invalid: spec.validation.openAPIV3Schema.properties[spec].properties[parameters]
.items.properties[valueFrom].properties[objectFieldRef].x-kubernetes-validations[0].rule:
compilation failed: ERROR: <input>:1:6: undefined field 'namespace'
 | !(has(self.namespace) && has(self.namespaceSelector))
```

The same rule appears under `status.resolvedParameters`. `namespace` **is**
declared as a property at that path, so this looks like a CEL type-resolution
difference between releases rather than a schema error. 1.33 accepts it, which
is why `ENVTEST_K8S_VERSION` is pinned to 1.33.0 here.

Worth confirming the supported floor and documenting it — anyone on a 1.31
cluster cannot install template-operator-v2 at all.

---

## MTO — unrelated defects (task A3)

Recorded here so they are not lost; they belong against `tenant-operator` and
`mto-extension-grafana` respectively.

1. The `Tenant` and `IntegrationConfig` conversion webhooks hardcode namespace
   `multi-tenant-operator`, so every v1beta2 request fails on a cluster where
   MTO runs elsewhere.
1. `mto-extension-grafana` declares `ScaffoldingModeOnLabel` and
   `DeletionPolicyOrphan` and reads neither.

## A4 — deprecate `integrations.argocd.*`

Docs plus a release note for `namespace`, `clusterResourceWhitelist` and
`namespaceResourceBlacklist`. Keep serving the fields. `accessControl.privileged`
is explicitly unaffected — it has five non-ArgoCD consumers and is core MTO
config, not part of this deprecation.
