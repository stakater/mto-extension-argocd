# Implementation tasks: mto-extension-argocd

> [!IMPORTANT]
> **Superseded in places — this is the design record, not the current behaviour.**
>
> These documents were written before the chart existed and before it was run on
> a cluster. Building and testing it reversed several decisions recorded here.
> Where they disagree with the chart, the chart is right. Known reversals:
>
> | Recorded here | Actual |
> |---|---|
> | `sync.mode: revert` by default | `strict`, with two mandatory `ignoreFields` exemptions |
> | `strict` needs tov2 >= 0.0.6 | the webhook ships in v0.0.3 |
> | child TemplateInstances named `mto-argocd-<tenant>` | `mto-argocd-tenant-<tenant>`, so a Tenant named `parent` cannot collide with the parent |
> | fixed cluster-scoped RBAC names | release-scoped, or a second install fails |
> | bash render harness copied from openbao | Go, mirroring template-operator-v2's own function map |
> | a blank `argocd.namespace` renders zero children | it renders one per tenant, each of which then fails |
> | the IntegrationConfig read is fail-closed as written | it failed OPEN until fixed; absence is now an error |
>
> Live behaviour is in [../live-deployment.md](../live-deployment.md); upstream
> defects are in [../upstream-issues.md](../upstream-issues.md).

> Derived from `PLAN-argocd-extension-extraction.md` after the 2026-08-06 config-model decisions (§12).
> Build order at the bottom. Nothing here is blocked on an open decision.

## A — External tickets (file first)

| ID | Task | Acceptance |
|---|---|---|
| A1 | **TOV2-1 — retain on delete** (template-operator-v2) | Implemented as a **delete-time policy check**, not by withholding ownership labels — withholding freezes a retained AppProject at creation-time content, and retain is the default. Retained resource carries no dangling finalizer (S37). Parent cascade honours the policy |
| A2 | MTO-1 — expose generated group names on `Tenant.status.groups` | Optional. The chart derives the hash itself; this only removes a derivation |
| A3 | File two unrelated defects | (a) Tenant + IntegrationConfig conversion webhooks hardcode namespace `multi-tenant-operator`, so every v1beta2 request fails where MTO runs elsewhere. (b) grafana declares `ScaffoldingModeOnLabel` and `DeletionPolicyOrphan` and reads neither |
| A4 | **Deprecate `integrations.argocd.*` in tenant-operator** | Docs + release note for `namespace`, `clusterResourceWhitelist`, `namespaceResourceBlacklist`. Keep serving the fields. `accessControl.privileged` explicitly unaffected |

## B — Repo and CI

| ID | Task | Acceptance |
|---|---|---|
| B1 | Create `mto-extension-argocd`, chart-only | Layout per §7.1. No `go.mod`, `cmd/`, `api/`, `config/`, `Dockerfile`, `bundle/`, `catalog/` |
| B2 | `Chart.yaml`, `values.yaml`, `values.schema.json` | Draft-07. `argocd.namespace` **required** (blank renders zero children and reaps every AppProject). `scaffolding.mode` enum. `sourceRepos.pattern` present and empty. Resource-list defaults. tov2 pinned ≥ 0.0.5 |
| B3 | CI harness copied from `mto-extension-openbao` | `make helm-lint` + `make helm-render-test` → `hack/chart-render-tests.sh`, fixtures in `hack/render-fixtures/`, `chart-tests` job in `pull_request.yaml`. No Go build, so no private-module auth |

## C — Templates

| ID | Task | Acceptance |
|---|---|---|
| C1 | Child template | From `spike-argocd-tov2/02-templates.yaml` — working code that produced a byte-identical AppProject. Use `fromJson` (`fromJsonArray` does not exist). Description → `Managed by mto-extension-argocd` |
| C2 | Per-tenant overrides from annotations | Keys under `argocd.tenantoperator.stakater.com/`. Absent inherits the chart default, `[]` overrides with empty (S22/S23). **Malformed JSON fails the child** — never falls back, or SSA strips the field from a live AppProject |
| C3 | `sourceRepos` resolution | Chart `pattern` with `{{ tenant }}` expanded per tenant; annotation overrides it; empty when neither is set. `pattern` only for now — a pattern without the placeholder is a literal |
| C4 | Roles fix | Groups = MTO generated group + IC privileged groups + the tenant's own groups **+ the tenant's users**. Order preserved for diffs. Without the users, vanilla-Kubernetes tenants get no ArgoCD access at all |
| C5 | Parent template | Three `objectFieldRef` params (Tenants, Namespaces, IC). IC narrowed to `accessControl.privileged`, `jsonPath: .spec`, **no `defaultValue`**. Scaffolding filter with configurable annotation/label keys. Unanchored `regexMatch` for privileged filtering. Computed values JSON-encoded |
| C6 | Parent aggregator output | `fromAll` over child TIs, `keyTemplate: "{.metadata.name}"`, per-condition fields |
| C7 | Child status and outputs | `exposeInStatus` on the namespaces param; outputs `argoNamespace`, `destinationCount` |
| C8 | `sync.mode` plumbing | Default `revert`. `ignoreFields` supported. `strict` documented as opt-in needing tov2 ≥ 0.0.6 and a `metadata.finalizers` exemption |
| C9 | RBAC | From `spike-argocd-tov2/01-rbac.yaml`. Cluster-wide read-only `list`/`watch`; every mutating verb namespace-scoped to the argo namespace. Reap verbs unconditional, `create` gated behind the toggle — otherwise disabling the feature deadlocks TI finalization |
| C10 | Retain branch of the child | **Blocked on A1** — wait for the agreed annotation key or write it twice |

## D — Tests

| ID | Task | Acceptance |
|---|---|---|
| D1 | **Privileged-namespace golden fixture — write before any other test** | The only untested path with a cross-tenant-exposure consequence, and MTO's managed-prefix webhook makes it untestable on a real cluster |
| D2 | Golden fixtures for the §11 minimum set | S21 zero-namespace tenant · S22/S23 nil-vs-empty **and** pure inheritance · S19 argo namespace always present · S26-S30 exact policies and group order |
| D3 | Cross-implementation equivalence test | Against `spike-argocd-tov2/fixtures/golden-appproject-pe.json`. That fixture is pre-roles-fix, so assert the one expected delta: `roles[].groups` gains the tenant's users |
| D4 | Golden files assert the **full** AppProject | Applies are server-side with `ForceOwnership`, so deleting a line from a template deletes that field from every live AppProject |
| D5 | E2E in the new repo | Re-express the current 469-line extensions suite |

## E — Migration

| ID | Task | Acceptance |
|---|---|---|
| E1 | Migration Job (Helm hook) | Strips `tenantoperator.stakater.com/v1beta2` Tenant ownerRefs from pre-existing AppProjects. Own SA + RBAC. **Must run before the parent adopts anything.** No Helm-hook precedent in the family — budget for getting SA/RBAC right from scratch |
| E2 | Phase 2 dual-run | Controller default-off and out of the `ENABLE_ALL_CONTROLLERS` branch. Seed the scaffolding marker from the existing `Extensions` set, per-tenant settings into annotations, and the IC's resource lists into chart values — all three, or the diff is not empty by construction. ≥ 1 week on staging |
| E3 | `README.md`, `MIGRATION.md`, `examples/` | Includes break-glass: set retain globally **before** `helm uninstall`, since uninstalling the parent reaps every child |
| E4 | Release notes | Delete old `Extensions` CRs with `onDeletePurgeAppProject: false` **while the controller still runs**, then remove the controller, then the CRD. Reversed, every CR sticks in `Terminating` and the CRD wedges |

## F — Removal from tenant-operator (Phase 3, once zero `Extensions` objects remain)

| ID | Task | Acceptance |
|---|---|---|
| F1 | Delete API and CRD surface | `api/v1alpha1/extensions_types.go` + deepcopy, CRD base + kustomization entry, webhook patch, editor/viewer roles, bundle manifest, CSV owned-CRD entry. **The `api/v1alpha1` package must survive** — legacy `IntegrationConfig` and `TemplateGroupInstance` live there |
| F2 | Delete controller and wiring | Package, `cmd/main.go:402-411`, config flags and env constant, `config/manager/extensions-argocd-deploy.yaml` + kustomization entry, the database controller's `Extensions` watch, `AppProjectCRDName`/`ArgoCDDefaultServer`, dead `ArgoCDSyncError`, dev plumbing, e2e suite entry |
| F3 | Remove the RBAC marker by hand | It is on the **Tenant** controller (`tenant_controller.go:62`), not the extensions package; `make manifests` will not remove it |
| F4 | Drop `argo-cd/v3` | Nine importers, including three e2e files that only register the scheme with no assertions |
| F5 | Follow-up PR | Drop the `gitops-engine` replace and the 34-line `k8s.io/* => v0.34.0` pin block. Largest dependency win; do it after the removal is green |

## Build order

**D1 → C1/C2/C3 → D2/D3 → B3 → C5/C9 → C4/C6/C7/C8 → E1 → A1 lands → C10 → E2 → F.**

- A1-A4 file immediately; only C10 and the Phase 2 rollout wait on A1.
- Do **not** re-verify the equivalence diff, adoption, the three-`objectFieldRef` parent, or the RBAC split
  — all measured on a live cluster and recorded in §3.
