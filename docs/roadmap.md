# Roadmap

Phases → stories. Story format, lifecycle and method as in `method.md`.
**📝 Draft** → **👍 Approved** → **🔨 In progress** → **✅ Done**.

## Phases

| Phase | Outcome |
|---|---|
| **1 · Runs today** ✅ | An AppProject per opted-in tenant, rendered by Template Operator, tested at four levels. |
| **2 · Migrate** 🔨 | Tenants move from the Extensions controller one at a time. |
| **0 · Prove the target** | The open questions answered on Argo CD 3.5 / OpenShift GitOps 1.22, before Phase 3–4 code. |
| **3 · Harden** | No tenant path into the control plane; least-privilege roles. **Do first.** |
| **4 · Current Argo CD tenancy** | Applications in the tenant's own namespace; sync impersonation; group-pattern identity; baseline Argo CD 3.5 / OpenShift GitOps 1.22. |
| **5 · Polish** | Sync windows, orphaned-resource warnings, named cluster resources. |

---

## Phase 1 — Runs today

**Story A.1 — An AppProject per tenant** ✅ *Done* (PR #1, 2026-08-06)

As a platform team, I want one AppProject per opted-in tenant derived from its
namespaces, repositories and access lists, so that nobody writes AppProjects
by hand. Satisfies **AC-ISO-1**, **AC-ISO-4**, **AC-ISO-5**, **AC-ACC-1**,
**AC-LIFE-1..5**, **AC-MIG-1**.

## Phase 2 — Migrate

**Story A.2 — Cut over from the Extensions controller** 🔨 *In progress*
(PR #2) — `hack/mto-argocd-migrate` and `MIGRATION.md`. **AC-MIG-2**.

---

## Phase 0 — Prove the target (before Phase 3–4 code)

**Story A.0 — Live check on Argo CD 3.5 / OpenShift GitOps 1.22** 📝 *Draft* —
answer the open questions in `argocd-baseline.md`, reproduce the control-plane
escalation (AC-ISO-3) on today's chart. Planning:
[`planning/A0-live-check.md`](planning/A0-live-check.md).

## Phase 3 — Harden (priority)

**Story A.3 — Take the Argo CD namespace out of tenant destinations** 📝 *Draft*
⚠️ security

As a security team, I want no tenant able to deploy into the Argo CD namespace,
so that no tenant can create Applications, AppProjects or credential Secrets
there through Argo CD's own controller.

Done: the Argo CD namespace is a destination only with
`argocd.allowControlPlaneDestination: true` (default false), and never when it
matches a privileged pattern; equivalence test updated with the documented
difference; release note. Satisfies **AC-ISO-3**. ADR-0006.

**Story A.4 — Least-privilege roles** 📝 *Draft* — `exec` off by default
(`roles.exec.enabled`); no `clusters` write by default; `repositories` write for
owner only; `acme-edit` without Application create, delete and override.
Satisfies **AC-ACC-2..4**. ADR-0009.

## Phase 4 — Current Argo CD tenancy

**Story A.5 — Applications in the tenant's own namespace** 📝 *Draft* —
`sourceNamespaces: [<tenant>-argocd]` (pattern configurable; never the control
plane or a privileged namespace); prerequisites documented
(`application.namespaces` or `ArgoCD.spec.sourceNamespaces`). **AC-SS-1**.
ADR-0007.

**Story A.6 — ApplicationSets in the tenant's namespace** 📝 *Draft* — documented
hardening (SCM provider allowlist, tokenRef strict mode); Argo CD 3.5 /
OpenShift GitOps 1.22.

**Story A.7 — Sync impersonation, opt-in** 📝 *Draft* —
`destinationServiceAccounts`, one per tenant namespace, ServiceAccount
`argocd-deployer` created by a Multi-Tenant Operator template and bound to the
namespace's edit role; no catch-all; Argo CD 3.5+. **AC-SS-2**. ADR-0008.

**Story A.8 — Group-pattern identity, optionally** 📝 *Draft* —
`identity.groupPattern` adds `tenant-{tenant}-{role}s` groups, matching the
other extensions; `accessControl` stays the default for compatibility.
**AC-ACC-5**. ADR-0010.

**Story A.9 — Baseline Argo CD 3.5 and OpenShift GitOps 1.22** 📝 *Draft* — e2e
on 3.5.x, version-gate table in the README.

## Phase 5 — Polish

**A.10** a recommended `namespaceResourceBlacklist` for kinds the Multi-Tenant
Operator owns (example, not default) · **A.11** per-tenant `syncWindows` and
`orphanedResources.warn` · **A.12** `name` on cluster-resource whitelist items
(Argo CD 3.3+) — all 📝 *Draft*.
