# Spike artifacts — ArgoCD extension on template-operator-v2

Working manifests and captured fixtures from the Phase 1 spike, run **2026-08-03** against a live
OpenShift 4.20 cluster (`160.79.117.160`) with MTO v1.8.2, template-operator-v2 v0.0.5, ArgoCD 3.4.6
and two tenants (`pe`, `sos`).

These are kept because the evidence is otherwise perishable: `golden-appproject-pe.json` is the output
of the **current Go controller**, and it is the reference the chart must reproduce. Once ArgoCD is
uninstalled from that cluster it cannot be regenerated without redoing the whole setup.

Everything here was applied, verified, and then removed from the cluster. Nothing is live.

## Contents

| File | What it is |
|---|---|
| `01-rbac.yaml` | The least-privilege split from PLAN §4.6, **verified working** — cluster-wide `list`/`watch` plus namespace-scoped mutation |
| `02-templates.yaml` | Working parent Template + child Template + parent TemplateInstance with three `objectFieldRef` parameters. Produced an AppProject byte-identical to the Go controller's |
| `fixtures/golden-appproject-pe.json` | **The reference output.** `AppProject/pe` as produced by `ExtensionsArgoCDReconciler` |
| `fixtures/input-tenant-pe.json` | The Tenant that produced it |
| `fixtures/input-integrationconfig.json` | The IntegrationConfig that produced it |
| `fixtures/input-namespaces-pe.json` | The ten namespaces labelled `stakater.com/tenant=pe` |

## What the spike proved

**The equivalence diff is empty.** Comparing the chart-rendered AppProject against the Go
controller's, across `clusterResourceWhitelist`, `description`, `destinations`,
`namespaceResourceBlacklist` and `roles`: identical. Eleven destinations both sides. Role groups
identical, including the sha256-derived names — `sha256sum (printf "%s-%s" $t $role) | trunc 6` in
sprig reproduces Go's `hex(sha256("<tenant>-<role>"))[:6]` exactly. Still identical after an
IntegrationConfig change.

**Three `objectFieldRef` parameters on one parent TI work** — two cluster-wide LISTs (`Tenant`,
`Namespace`) plus one namespaced GET (`IntegrationConfig` at `jsonPath: .spec`). All resolved
(`AllParametersResolved`), and **all three register watches**:

| Trigger | Result |
|---|---|
| Namespace created with `stakater.com/tenant=sos` | parent re-rendered, `sos` child's namespace list grew, AppProject destinations grew |
| Tenant annotated | parent re-rendered, child emitted, AppProject created |
| IntegrationConfig patched | child's `whitelistJson` updated, AppProject whitelist updated |

**Only the changed child is rewritten.** After a namespace event affecting `sos` only: `pe`'s child TI
was untouched (resourceVersion unchanged), `sos`'s went to generation 2. One tenant edit costs one
write, not N.

**The split RBAC works, including deletion.** tov2 created *and deleted* AppProjects in the argo
namespace through a `RoleBinding`, while `create`/`delete` in `default` stayed denied and cluster-wide
`list` (needed by `discoverOwnedResources`) still worked.

**`scaffoldingMode` works end to end.** `Always` scaffolded both tenants; switching to `OnAnnotation`
reaped both children *and both AppProjects*; annotating `pe` brought exactly `pe` back.

## Gotchas found the hard way

**`fromJsonArray` does not exist.** Probed on the deployed version: `fromJson` ✅, `mustFromJson` ✅,
`fromJsonArray` ❌, `fromYaml` ❌ (the last is a Helm function, not sprig). `fromJson` decodes a
top-level JSON array correctly, so use that. `02-templates.yaml` uses `fromJson` throughout.

**JSON encoding of list parameters is mandatory, not stylistic.** Tenant `sos` has zero labelled
namespaces and its parameter resolved as the string `"[]"` — fine. A comma-joined empty list would be
`""`, which `ResolveParameterValue` treats as *unset* and rejects with `no value or valueFrom is set`.
The zero-namespace tenant is the common case that breaks.

**Flipping one chart value destroyed two tenants' AppProjects.** Switching `Always` → `OnAnnotation`
cascaded through orphan reaping and deleted both. This is the TOV2-1 gap, demonstrated: there is no way
today to say "stop managing this, but do not delete it".

**`kubectl wait --for=jsonpath` fails fast on an out-of-bounds index** rather than retrying, which
twice made a working propagation look broken. Re-read the object before concluding anything.

## Not tested

`sync.mode: strict`. The deployed CRD advertises the `off`/`revert`/`strict` enum, but the running
image is **v0.0.5** and OpenBao's `Chart.yaml` records `strict` as needing tov2 `>= 0.0.6`. CRD schema
and controller version can skew, so the enum's presence proves nothing. If the chart adopts `strict`
it must require `>= 0.0.6`, and exempt `metadata.finalizers` via `ignoreFields`.

## Reproducing

```bash
kubectl apply -f 01-rbac.yaml          # grant tov2 what it needs
kubectl apply -f 02-templates.yaml     # parent + child + parent TI
kubectl get templateinstances.templates.v2.stakater.com -n stakater-tenant-operator
kubectl get appproject -n stakater-tenant-operator      # expect spike-<tenant>
```

Renders to `spike-<tenant>` deliberately, so it never contends with the live
`ExtensionsArgoCDReconciler` over `AppProject/<tenant>`. Requires the argo namespace to match
`.spec.integrations.argocd.namespace` in the IntegrationConfig.
