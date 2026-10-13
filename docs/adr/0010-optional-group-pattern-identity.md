# ADR-0010: Optional group-pattern identity

- **Status:** Accepted (2026-10-12)
- **Date:** 2026-10-09

## Context

Other extensions take people's access from identity-provider groups named by
pattern (`tenant-{tenant}-{tier}s`), never from `Tenant.spec.accessControl`.
This chart uses `accessControl` for equivalence (ADR-0003). Under OpenShift
external OIDC, the MTO-generated OpenShift group doesn't reach Argo CD (I4);
groups named like local users inherit their roles (I2).

## Decision

`identity.groupPattern` (default empty). When set, each role gets the pattern's
group (`tenant-acme-owners`, `-editors`, `-viewers`) — alone, or alongside
`accessControl` with `identity.mode: both` during migration.

## Consequences

- One identity model across extensions for new installs.
- Existing installs keep working unchanged.

## Alternatives

- **Switch outright.** Rejected: breaks access for migrating customers.
