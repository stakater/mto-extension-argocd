# Live deployment

There is a **running install of this chart** on the shared OpenShift cluster.
It is deliberately left up. Do not tear it down without telling whoever is
using it.

Last deployed 2026-08-06.

## Where

| | |
|---|---|
| Cluster | `160.79.117.160`, OpenShift 4.20 (k8s v1.33.5) |
| kube context | `default/160-79-117-160:6443/ayesha.arshad@stakater.com` |
| Helm release | `mto-argocd` in `stakater-tenant-operator` |
| template-operator-v2 | v0.0.5, `stakater-template-operator` |
| ArgoCD | 3.4.6, `stakater-tenant-operator` |
| MTO | v1.8.2 |

**Always pass `--context` explicitly.** The ambient `current-context` on this
machine switches on its own, and several commands here mutate real objects.

## What is installed

```bash
CTX=default/160-79-117-160:6443/ayesha.arshad@stakater.com
helm --kube-context="$CTX" get values mto-argocd -n stakater-tenant-operator
```

```yaml
argocd:
  namespace: stakater-tenant-operator
  # Seeded from the live IntegrationConfig's deprecated integrations.argocd,
  # which is exactly what a real migration has to do. Without this the rendered
  # AppProjects silently lose both lists.
  clusterResourceWhitelist:
    - {group: tronador.stakater.com, kind: EnvironmentProvisioner}
    - {group: "", kind: PersistentVolume}
  namespaceResourceBlacklist:
    - {group: "", kind: ResourceQuota}
operatorNamespace: stakater-tenant-operator
integrationConfig: {namespace: stakater-tenant-operator}
rbac: {serviceAccount: {namespace: stakater-template-operator}}
scaffolding: {mode: OnAnnotation}
# sync.mode: strict, by chart default
```

`argocd.namespace` and `operatorNamespace` are both the ArgoCD namespace here
because that is where ArgoCD actually runs on this cluster. They are
independent values; nothing requires them to match.

## Which tenants are opted in

Only **`sos`**. `pe` was migrated on 2026-08-07 to verify the runbook and then
rolled back the same day, so the cluster is in its original state — see
[Migration, verified end to end](#migration-verified-end-to-end).

**`pe` must NOT be opted in while `Extensions/pe-extensions` exists.** Two
writers would claim `AppProject/pe`; the chart wins and the controller
error-loops where nobody is looking. `scaffolding.mode` is `OnAnnotation`
precisely so that cannot happen by accident — never set it to `Always` here.

The migration deletes the CR *first*, which is what makes opting `pe` in safe
during a real cutover.

## Inspecting it

```bash
CTX=default/160-79-117-160:6443/ayesha.arshad@stakater.com
NS=stakater-tenant-operator

# the parent and one child per opted-in tenant
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com -n $NS

# which AppProjects are ours (ours carry an instance-ref annotation)
kubectl --context="$CTX" get appprojects.argoproj.io -n $NS -o json \
  | jq -r '.items[] | "\(.metadata.name) ours=\(.metadata.annotations["templates.v2.stakater.com/instance-ref"] // "no")"'

# what a tenant got
kubectl --context="$CTX" get appprojects.argoproj.io -n $NS sos -o yaml
```

**Use fully-qualified resource names.** OpenShift ships its own
`template.openshift.io/TemplateInstance`, so bare `kubectl get templateinstance`
returns the wrong kind and an empty status — which looks exactly like "the
operator is not reconciling".

## Current state

| Object | Owner |
|---|---|
| `AppProject/sos` | this chart |
| `Application/sos-allowed-dest` | a live demonstration, see below — manual sync, deploys nothing |
| `AppProject/pe` | the Go controller — migrated to the chart on 2026-08-07 and **rolled back the same day** |
| `AppProject/default` | ArgoCD's own — leave alone |
| `Extensions/pe-extensions` | restored by the rollback; healthy, `AppProjectSynced` |

Tenant `sos` also carries a `source-repos` annotation, added to make the
project usable — see the finding below. `pe` does not, so its project still
permits no repositories — faithfully, which is the point.

## Migration, verified end to end

Run on 2026-08-07 with [hack/mto-argocd-migrate](../hack/mto-argocd-migrate),
following [MIGRATION.md](../MIGRATION.md). Tenant `pe` was the subject: a live
`Extensions/pe-extensions` CR and a controller-owned `AppProject/pe`, which is
exactly the before-state a customer has.

| Step | Result |
|---|---|
| `inventory` | Read the chart's config off `Template/mto-argocd-parent`, matched `argocd.namespace` against the IntegrationConfig, and found no unseeded resource lists |
| `strip-owner-refs` | Removed a `v1beta2` `Tenant` ownerRef and left the AppProject intact |
| `snapshot` | 3 AppProjects captured; `diff` against it immediately after was clean |
| `cutover pe --apply` | Deleted the CR, **confirmed `AppProject/pe` survived**, opted the tenant in, waited for `mto-argocd-tenant-pe` to go `Ready` |
| `diff` | `1 changed, 0 new, 0 missing` — only the description and the added user |

**The load-bearing claim now has evidence.** Deleting an `Extensions` CR does
not take its AppProject with it, and the finalizer
`tenantoperator.stakater.com/appproject` was released cleanly by the still-running
controller.

**Adoption is in place, not a recreate.** `AppProject/pe` kept UID
`e5401603-2209-4c05-872e-b63ff3166139` across the whole procedure. It gained
template-operator-v2's ownership labels and finalizer, and **kept** the
controller's own `multi-tenant-operator/predicate` label — server-side apply
does not remove a field owned by another manager, so stale metadata from the old
owner survives adoption. Harmless here; worth knowing before assuming an adopted
object is clean.

The whole diff, and nothing else:

```diff
-    "description": "Managed by Extensions controller",
+    "description": "Managed by mto-extension-argocd",
       "groups": [
-          "pe-owner-group-489fab"
+          "pe-owner-group-489fab",
+          "ayesha.arshad@stakater.com"
```

`destinations` (11), `clusterResourceWhitelist` (2) and
`namespaceResourceBlacklist` (1) were all unchanged, which is what proves the
step-3 seeding worked — the chart no longer reads them from the
IntegrationConfig, so an unseeded install would have stripped both lists here.

### Two things this run did NOT cover

- **The `onDeletePurgeAppProject: true` → `false` patch.** `pe-extensions` had no
  `spec.argoCD` block at all, so it took the nil-config retain branch rather than
  the purge branch. Arming purge on a live AppProject to test the other branch was
  judged too dangerous to do here. Still covered only by `tests/migrate`.
- **A tenant with per-tenant overrides.** `pe-extensions` set none, so
  `annotations` had nothing to carry across. Also covered only by `tests/migrate`.

### Rollback, also verified — and `pe` is back on the controller

`pe` was migrated and then rolled back on the same day, so the cluster is once
again in its pre-migration state. The rollback path from MIGRATION.md, run
exactly as written:

```bash
kubectl apply -f pe-extensions.restore.yaml   # controller error-loops; expected
kubectl annotate tenant pe argocd.tenantoperator.stakater.com/enabled-
```

Step 1 reproduced the denial verbatim, which is what leaves the controller in the
retry loop that makes step 2 self-heal:

```text
ERROR Reconciler error  Extensions: pe-extensions
  failed to create or update AppProject: admission webhook
  "sync-enforcement.templates.v2.stakater.com" denied the request: resource is
  managed by sync-enabled TemplateInstance stakater-tenant-operator/mto-argocd-tenant-pe
```

Timeline after removing the annotation, sampled every 4s:

```text
t+4s   chart-managed, uid=e5401603...
t+8s   ABSENT
t+12s  ABSENT
t+16s  controller-managed, uid=8bff29cd...
```

| | |
|---|---|
| Gap with no AppProject | **~8 seconds** |
| Content afterwards | `0 changed, 0 new, 0 missing` against the pre-migration snapshot |
| tov2 residue | none — no finalizer, label, annotation or ownerRef |
| `metadata.uid` | **changed** |

**The UID change is the finding.** A rollback is a delete-and-recreate, not a
restore, so `spec.roles[].jwtTokens` — the ArgoCD project role tokens minted by
`argocd proj role create-token` — are destroyed with the object. Forward
migration keeps the UID; rollback does not. Recorded in MIGRATION.md.

## When an AppProject gets deleted

The chart's failure modes are mostly destructive rather than degraded, because
template-operator-v2 reaps anything the parent stops rendering. Everything below
was checked on this cluster unless marked otherwise.

**Deletes the AppProject:**

| Trigger | Why |
|---|---|
| The opt-in annotation is removed | the tenant fails the scaffolding filter, no child is emitted, the orphan is reaped |
| `scaffolding.mode` is narrowed | same, for every tenant the new mode excludes |
| The Tenant is deleted or renamed | same — a renamed Tenant orphans its old child |
| The child TemplateInstance is renamed | a chart-side change; the old child orphans. Cost one delete-and-recreate per tenant when children moved to the `mto-argocd-tenant-` prefix |
| `helm uninstall`, or deleting the parent TemplateInstance | reaps every child at once — and deadlocks unless done in the order in NOTES.txt (TOV2-5) |

**Does NOT delete it** -- each of these was tried:

| Situation | Outcome |
|---|---|
| A malformed override annotation | that child fails; the live AppProject is untouched, `resourceVersion` unchanged |
| A tenant loses all its namespaces | keeps its AppProject, with the ArgoCD namespace as its only destination (S21) |
| The parent cannot render at all | **children and their AppProjects survive.** Pointing `integrationConfig.name` at a non-existent object put the parent in `ParametersValid=ParameterResolutionFailed`, and the child kept the same UID throughout. tov2 holds the last rendered set rather than treating "cannot render" as "render nothing" — so a broken IntegrationConfig does not wipe the fleet |
| A chart upgrade, values change or template edit | updated in place via server-side apply — same UID, only `resourceVersion` moves |
| `onDeletePurgeAppProject: false` | **does not protect anything on this cluster** — tov2 v0.0.5 ignores the retain annotation. Honoured from `8a44194` (TOV2-1), verified on the kind E2E cluster |

The last row of the first table is the one to plan around: renaming a child is a
breaking change. It must be done reap-first, and it earns a release note.

## Reading the status

### Per tenant — list the children

Each opted-in tenant has its own child TemplateInstance, so the list is already
one row per tenant. tov2's CRD defines no printer columns (TOV2-7), so ask for
them:

```bash
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com \
  -n stakater-tenant-operator -l templates.v2.stakater.com/instance \
  -o custom-columns='TENANT:.spec.parameters[?(@.name=="tenant")].value,\
READY:.status.conditions[?(@.type=="Ready")].status,\
REASON:.status.conditions[?(@.type=="Ready")].reason'
```

```text
TENANT   READY   REASON
sos      True    Reconciled
```

The `-l templates.v2.stakater.com/instance` selector matters: tov2 stamps that
label on the children it renders, so it excludes the Helm-installed parent,
which would otherwise show up with an empty TENANT.

This is the view to reach for first. The parent's aggregate below answers a
different question.

### Across the fleet — the parent's roll-up

The parent aggregates every child, so one object answers for the whole fleet:

```bash
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com \
  -n stakater-tenant-operator mto-argocd-parent -o json | jq '.status.outputs'
```

Healthy:

```json
{"ready":   {"mto-argocd-sos": "[\"True\"]"},
 "reason":  {"mto-argocd-sos": "[\"Reconciled\"]"},
 "errors":  {"mto-argocd-sos": "[]"}}
```

Broken — here a tenant with a malformed override annotation:

```json
{"ready":  {"mto-argocd-sos": "[\"False\"]"},
 "reason": {"mto-argocd-sos": "[\"TemplateExecutionFailed\"]"},
 "errors": {"mto-argocd-sos": "[\"failed to execute template: ... error calling
             mustFromJson: invalid character 'b' looking for beginning of object
             key string\",\"Template rendering failed\"]"}}
```

`ready` is what you alert on, `reason` is what you group by, `errors` is what
you read. `errors` selects conditions by **status**, not by type, on purpose:
the `Ready` condition only ever summarises ("Template rendering failed"), while
the cause sits on whichever stage actually broke — `Rendered` here. Selecting
`status==False` gets the real message whichever stage it was, and is empty
while the tenant is healthy, so it costs nothing on a working fleet.

### With more than one tenant

Every field is a map keyed by child name, so each tenant adds a row to each map
rather than changing the shape. With three tenants, one of them broken, kubectl
prints exactly this:

```yaml
status:
  outputs:
    errors:
      mto-argocd-sos:   '[]'
      mto-argocd-alpha: '[]'
      mto-argocd-beta:  '["failed to execute template: template: template:36:11: executing \"template\" at <mustFromJson .parameters.whitelistJson>: error calling mustFromJson: invalid character ''b'' looking for beginning of object key string","Template rendering failed"]'
    ready:
      mto-argocd-sos:   '["True"]'
      mto-argocd-alpha: '["True"]'
      mto-argocd-beta:  '["False"]'
    reason:
      mto-argocd-sos:   '["Reconciled"]'
      mto-argocd-alpha: '["Reconciled"]'
      mto-argocd-beta:  '["TemplateExecutionFailed"]'
```

(The `sos` rows are real; `alpha` and `beta` were synthesized into the live
object so the quoting and serialization are exactly what kubectl produces. The
cluster's MTO licence caps it at two Tenants, so a genuine three-tenant status
cannot be produced there.)

Three things follow:

- **The field count is fixed, the rows grow.** Always three maps, one line each
  per tenant. Fifty tenants is 150 lines across three blocks.
- **Reading one tenant means correlating across all three maps by key.** Above,
  `beta` is broken -- you see that in `ready`, and find out why in `errors`,
  which at any real scale is far away.
- **Error strings land verbatim**, with YAML quote-escaping. One broken
  tenant's message outweighs ten healthy tenants' entries put together.

Size is not the constraint -- even 200 tenants is tens of KB, well inside
etcd's limit. Readability is. So use the per-tenant child list above for "how is
each tenant doing", and this object only for "is anything broken at all".

One line per tenant:

```bash
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com \
  -n stakater-tenant-operator mto-argocd-parent -o json \
  | jq -r '.status.outputs as $o | $o.ready | keys[] as $k
      | "\($k | sub("^mto-argocd-";""))  \($o.ready[$k]|fromjson|join(""))  \($o.reason[$k]|fromjson|join(""))"'
```

```text
sos  True  Reconciled
```

Only the broken ones, with the cause — silent when the fleet is healthy, which
is the one you want in a runbook or an alert:

```bash
... | jq -r '.status.outputs as $o | $o.ready | keys[] as $k
      | select(($o.ready[$k]|fromjson|join("")) != "True")
      | "\($k | sub("^mto-argocd-";"")): \($o.errors[$k]|fromjson|join(" | "))"'
```

A count by state, for a dashboard:

```bash
... | jq -r '[.status.outputs.ready[] | fromjson | join("")]
      | group_by(.) | map("\(.[0])=\(length)") | join("  ")'
```

```text
True=1
```

#### Why it is not grouped by tenant

The obvious shape would be tenant-first:

```yaml
sos:
  ready: "True"
  reason: Reconciled
  errors: []
```

template-operator-v2 cannot produce that. The status field is typed

```go
Outputs map[string]map[string]string   // field -> key -> value
```

Two levels, both strings, written as `outputs[field][key] = value`. The nesting
order is fixed in the CRD schema; there is no per-output switch to invert it,
and tenant-first would need a third level, which is a breaking API change.
Raised upstream as TOV2-6.

Until then, invert it client-side:

```bash
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com \
  -n stakater-tenant-operator mto-argocd-parent -o json \
  | jq '.status.outputs as $o
      | reduce ($o.ready|keys[]) as $k ({};
          .[$k|sub("^mto-argocd-";"")] = {
            ready:  ($o.ready[$k]  | fromjson | join("")),
            reason: ($o.reason[$k] | fromjson | join("")),
            errors: ($o.errors[$k] | fromjson)
          })' | yq -P
```

```yaml
sos:
  ready: "True"
  reason: Reconciled
  errors: []
```

Note the values are JSON-encoded **strings**, not arrays — a JSONPath output
field always yields a list, and template-operator-v2 stores it encoded. Hence
the `fromjson|join("")` in every recipe.

Per-tenant detail lives on the child, which also reads back what actually
landed on the cluster:

```bash
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com \
  -n stakater-tenant-operator mto-argocd-sos -o json | jq '.status.outputs'
# {"destinations": {"sos": "[\"stakater-tenant-operator\"]"}}
```

`destinations` is read from the live AppProject, not from the template, so it
confirms what privileged filtering actually left behind.

### Compared with the old Extensions CR status

| `Extensions.status.argocd` | Now |
|---|---|
| `appProjectState: true` | the child's `Ready` condition |
| `argoNamespace` | `argoNamespace`, exposed on the child |
| one `AppProjectSynced` condition | eight conditions, plus `errors` on the parent |
| — | `destinations`, read back from the live object |
| — | the resolved namespace list |
| — | a fleet-wide view; the old CR only described one tenant |

## What ArgoCD actually enforces — verified, not assumed

ArgoCD here has no Route and `argocd-server` is `ClusterIP`, so there is no UI.
Reach it with:

```bash
kubectl --context="$CTX" port-forward -n stakater-tenant-operator \
  svc/argocd-server 8080:443
```

An AppProject existing does not prove ArgoCD honours it, so two `Application`
objects were created under project `sos` — manual sync, so neither deploys
anything — and ArgoCD's verdict read back.

**Destinations are enforced.** An Application targeting namespace `default`,
which is not in `sos`'s destinations, was refused:

```text
InvalidSpecError: application destination server 'https://kubernetes.default.svc'
and namespace 'default' do not match any of the allowed destinations in project 'sos'
```

The Application targeting `stakater-tenant-operator` — the one destination
`sos` has — was accepted, and reached `OutOfSync / Missing`, meaning ArgoCD
fetched the repo and is waiting for a manual sync. That is the whole chain
proven end to end: Tenant annotation → parent → child TemplateInstance →
AppProject → ArgoCD accepting a real Application inside it.

### The finding that matters for rollout

**An AppProject with no `sourceRepos` permits no repositories at all**, so the
tenant cannot deploy anything:

```text
InvalidSpecError: application repo https://github.com/argoproj/argocd-example-apps
is not permitted in project 'sos'
```

This is **not** something this chart introduces. Both projects had
`sourceRepos: null` — the one this chart rendered *and* `AppProject/pe`, which
`ExtensionsArgoCDReconciler` produced. `sourceRepos` was `required` on the
v1beta2 `Tenant` and became optional on the `Extensions` CR, so any tenant that
never set it has a project ArgoCD will not deploy from.

The consequence for Phase 2: byte-identical migration is the correct goal, and
it will faithfully reproduce non-functional projects for every tenant that has
no `sourceRepos` today. **"The AppProject exists" is not the same as "the tenant
can use ArgoCD."** Plan the `sourceRepos` rollout as its own step, after the
equivalence diff is confirmed empty.

Setting it live fixed the project immediately:

```bash
kubectl --context="$CTX" annotate tenant sos \
  'argocd.tenantoperator.stakater.com/source-repos=["https://github.com/argoproj/argocd-example-apps"]' \
  --overwrite
```

### What is still NOT proven

The three roles (`sos-owner`, `sos-edit`, `sos-view`) and their policy strings
are rendered correctly and accepted by ArgoCD, but nobody has logged in as a
member of those groups to confirm the access they grant. That needs SSO plus
ingress, neither of which is set up here.

## Opting another tenant in or out

```bash
kubectl --context="$CTX" annotate tenant <name> \
  argocd.tenantoperator.stakater.com/enabled=true --overwrite
```

The value must be exactly `"true"`. **Removing the annotation deletes that
tenant's AppProject** on this cluster — tov2 v0.0.5 does not enforce retain
(TOV2-1).

## Removing it, when that time comes

Not a plain `helm uninstall`. That deletes the RBAC and the TemplateInstances
together; the RBAC goes immediately while each TemplateInstance waits on a
finalizer that needs the RBAC to delete its AppProject, so nothing completes and
every object needs its finalizer cleared by hand (TOV2-5).

```bash
# 1. drop the parent; children and their AppProjects drain while RBAC exists
kubectl --context="$CTX" delete templateinstances.templates.v2.stakater.com \
  mto-argocd-parent -n stakater-tenant-operator

# 2. wait until no mto-argocd-* children remain
kubectl --context="$CTX" get templateinstances.templates.v2.stakater.com -n stakater-tenant-operator

# 3. now the release
helm --kube-context="$CTX" uninstall mto-argocd -n stakater-tenant-operator

# 4. and the opt-in marker
kubectl --context="$CTX" annotate tenant sos argocd.tenantoperator.stakater.com/enabled-
```

`helm uninstall` also returns before template-operator-v2 releases its
finalizer on `Template/mto-argocd-parent`; wait for that object to disappear
before reinstalling, or the install fails with an ownership-metadata error
(TOV2-4).

## Re-running E2E against this install

**Most of the time you want `make e2e-kind` instead** — it builds its own
cluster and touches nothing here ([docs/e2e.md](e2e.md)). Run against *this*
cluster only for what kind cannot show you: that the chart coexists with a real
multi-tenant-operator, on OpenShift, with real tenants.

The E2E suite installs its own release, so uninstall this one first or point it
at a different `E2E_OPERATOR_NAMESPACE`:

```bash
E2E=1 \
KUBE_CONTEXT="default/160-79-117-160:6443/ayesha.arshad@stakater.com" \
E2E_TENANT=sos \
E2E_ARGO_NAMESPACE=stakater-tenant-operator \
E2E_TOV2_SA=system:serviceaccount:stakater-template-operator:template-operator-v2 \
make e2e-test
```

## Constraints of this cluster

- **MTO free tier caps it at two Tenants**, and both (`pe`, `sos`) are used. You
  cannot create test tenants here.
- **MTO's namespace webhook refuses any namespace without a
  `stakater.com/tenant` label**, so you cannot create a scratch namespace
  either.

Together these are why the E2E suite borrows an existing tenant here instead of
building its own footprint — and why the kind cluster exists: with MTO's CRDs
but not MTO, the suite creates its own tenants and can reach the two paths this
cluster forbids, a privileged namespace labelled for a tenant and a tenant with
no namespaces at all.
