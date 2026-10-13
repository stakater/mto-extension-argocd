# Roadmap

Phases → stories. Story format, lifecycle and method as in `method.md`.
**📝 Draft** → **👍 Approved** → **🔨 In progress** → **✅ Done**.

## Phases

| Phase | Outcome |
|---|---|
| **1 · Runs today** ✅ | An AppProject per opted-in tenant, rendered by Template Operator, tested at four levels. |
| **2 · Migrate** 🔨 | Tenants move from the Extensions controller one at a time. |
| **0 · Prove the target** 📝 | The open questions answered on Argo CD 3.5 / OpenShift GitOps 1.22. Planned before Phase 3–4 code; the code shipped first, so A.0 now confirms it on live clusters. |
| **3 · Harden** ✅ | No tenant path into the control plane; least-privilege roles. Chart 0.2.0. |
| **4 · Current Argo CD tenancy** ✅ | Applications in the tenant's own namespace; sync impersonation; group-pattern identity; baseline Argo CD 3.5 / OpenShift GitOps 1.22. Charts 0.3.0–0.5.0 and 0.7.1. |
| **5 · Polish** ✅ | Sync windows, orphaned-resource monitoring, named cluster resources. Charts 0.6.0–0.7.1. |

---

## Phase 1 — Runs today

**Story A.1 — An AppProject per tenant** ✅ *Done* (PR #1, 2026-08-06)

As a platform team, I want one AppProject per opted-in tenant derived from its
namespaces, repositories and access lists, so that nobody writes AppProjects
by hand. Satisfies **AC-ISO-1**, **AC-ISO-4**, **AC-ISO-5**, **AC-ACC-1**,
**AC-LIFE-1..5**, **AC-MIG-1**.

## Phase 2 — Migrate

**Story A.2 — Cut over from the Extensions controller** 🔨 *In progress*
(PR #2) — `hack/mto-argocd-migrate` and `MIGRATION.md`. **AC-MIG-2**. Starts
from the compatibility values `argocd.allowControlPlaneDestination: true` and
`roles.profile: extensionsCompatible`, which reproduce the controller's
AppProjects exactly.

---

## Phase 0 — Prove the target

**Story A.0 — Live check on Argo CD 3.5 / OpenShift GitOps 1.22** 📝 *Draft*
(#8) — answer the open questions in `argocd-baseline.md`; reproduce the
control-plane escalation (AC-ISO-3) on chart 0.1.0 and confirm 0.2.0+ blocks
it. Planning: [`planning/A0-live-check.md`](planning/A0-live-check.md).

## Phase 3 — Harden

**Story A.3 — Take the Argo CD namespace out of tenant destinations** ✅ *Done*
(PR #17, chart 0.2.0, 2026-10-12) ⚠️ security

As a security team, I want no tenant able to deploy into the Argo CD namespace,
so that no tenant can create Applications, AppProjects or credential Secrets
there through Argo CD's own controller.

Built: the Argo CD namespace is never a destination, not even when labelled for
a tenant. `argocd.allowControlPlaneDestination: true` restores the legacy shape
exactly, for a migration only. Satisfies **AC-ISO-3**. ADR-0006.

**Story A.4 — Least-privilege roles** ✅ *Done* (PR #17, chart 0.2.0) —
`exec` off by default (`roles.exec.enabled`, owner only); no `clusters` write
by default (`roles.clusters.enabled`); `repositories` write for owner only;
`acme-edit` without Application create, delete and override.
`roles.profile: extensionsCompatible` keeps the legacy shape for migration.
Satisfies **AC-ACC-2..4**. ADR-0009.

## Phase 4 — Current Argo CD tenancy

**Story A.5 — Applications in the tenant's own namespace** ✅ *Done* (PR #18,
chart 0.3.0) — `sourceNamespaces: [<tenant>-argocd]`, opt-in. The pattern must
contain `{{ tenant }}` and expand to a plain name (no globs), and the namespace
is listed only while it exists with the tenant's label. Never the control plane
or a privileged namespace. Prerequisites documented (`application.namespaces`
or `ArgoCD.spec.sourceNamespaces`). **AC-SS-1**. ADR-0007.

**Story A.6 — ApplicationSets in the tenant's namespace** ✅ *Done* (PR #23,
in chart 0.7.1) — documented hardening (SCM provider allowlist, tokenRef strict
mode); Argo CD 3.5 / OpenShift GitOps 1.22.

**Story A.7 — Sync impersonation, opt-in** ✅ *Done* (PR #19, chart 0.4.0) —
`destinationServiceAccounts`, one per tenant namespace, ServiceAccount
`argocd-deployer` created by a Multi-Tenant Operator template and bound to the
namespace's edit role; no catch-all; Argo CD 3.5+. **AC-SS-2**. ADR-0008.

**Story A.8 — Group-pattern identity, optionally** ✅ *Done* (PR #20, chart
0.5.0) — `identity.mode: groupPattern` or `both` adds
`tenant-<tenant>-<owners|editors|viewers>` (`identity.groupPattern`), matching
the other extensions; `accessControl` stays the default for compatibility.
**AC-ACC-5**. ADR-0010.

**Story A.9 — Baseline Argo CD 3.5 and OpenShift GitOps 1.22** ✅ *Done*
(PR #23, in chart 0.7.1) — e2e on 3.5.4, version-gate table in the README.

## Phase 5 — Polish

**A.10** a recommended `namespaceResourceBlacklist` for kinds the Multi-Tenant
Operator owns (example, not default; PR #23) · **A.11** per-tenant
`syncWindows` and `orphanedResources: off|warn|monitor` (PR #21, chart 0.6.0)
· **A.12** `name` on cluster-resource whitelist items, Argo CD 3.3+, behind
`argocd.allowClusterResourceNames` (PR #22, chart 0.7.1) — all ✅ *Done*.
