# Upstream issues found while building this chart

Drafts ready to file against `template-operator-v2`. Every one was reproduced
on a live OpenShift 4.20 cluster (MTO v1.8.2, tov2 v0.0.5, ArgoCD 3.4.6); the
reproduction steps are exact.

---

## TOV2-1 — retain on delete

**Status:** known, unfiled. Blocks `C10` and the Phase 2 rollout.

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
