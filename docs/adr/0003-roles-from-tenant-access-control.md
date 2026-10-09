# ADR-0003: Roles from the Tenant's access lists, for equivalence

- **Status:** Accepted (built, PR #1); an alternative source is proposed in
  ADR-0010
- **Date:** 2026-08-06

## Context

The chart replaces the Extensions controller, whose AppProject roles took
groups from `Tenant.spec.accessControl` and the IntegrationConfig's privileged
groups. Customers rely on that.

## Decision

Role `groups`: the MTO-generated OpenShift group, privileged groups, the
tenant's groups, then its users — order kept for the equivalence test. Users
are added (a fix: the generated group only exists on OpenShift).

## Consequences

- Migration is a no-op for access.
- User entries need argocd-rbac-cm `scopes` to include the user-name claim (I1).
- Differs from the other extensions, which take access from groups named by
  pattern (ADR-0010).

## Alternatives

- **Groups by pattern only.** Deferred to ADR-0010: would change access during
  migration.
