# ADR-0009: Least-privilege project roles

- **Status:** Proposed — **security, priority**
- **Date:** 2026-10-09

## Context

Owner and edit both get `*` on all six project resources (R3). `exec` runs as
the argocd-server ServiceAccount, outside the tenant's RBAC and audit (R2).
`clusters` write lets tenants register clusters (T1). `exec, get` for view does
nothing.

## Decision

- `exec` only with `roles.exec.enabled`, owner only.
- `clusters` write only with `roles.clusters.enabled`; then also
  `permitOnlyProjectScopedClusters: true`.
- `repositories` write for owner; `get` for edit and view.
- Edit: `get`, `sync`, `update`, `update/*`, `action/*` on applications; no
  create, delete or override.
- Recommend admins set `requireOverridePrivilegeForRevisionSync` (V5).

## Consequences

- Edit becomes meaningfully narrower; a documented difference from the
  Extensions controller.
- Teams relying on editors creating Applications move them to owner.

## Alternatives

- **Keep equivalence.** Rejected for new installs; possible as a compatibility
  value during migration.
