# ADR-0002: One AppProject per tenant, destinations listed explicitly

- **Status:** Accepted (built, PR #1); the control-plane destination is
  revisited by ADR-0006
- **Date:** 2026-08-06

## Context

The AppProject is Argo CD's tenant unit (sourceRepos, destinations, resource
lists, roles). Destinations accept globs and negation (T3).

## Decision

One AppProject per opted-in tenant, named after it, in `argocd.namespace`.
Destinations list each non-privileged tenant namespace explicitly; privileged
namespaces (IntegrationConfig, unanchored regex) are excluded.

## Consequences

- A new namespace becomes a destination on the next render.
- A glob like `acme-*` would be simpler but could match namespaces the tenant
  doesn't own; explicit lists don't.

## Alternatives

- **Glob destinations.** Rejected: trusts naming, not ownership.
