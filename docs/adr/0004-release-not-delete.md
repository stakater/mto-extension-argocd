# ADR-0004: Removing management releases the AppProject by default

- **Status:** Accepted (built, PR #1)
- **Date:** 2026-08-06

## Decision

`onDeletePurgeAppProject: false` (default) marks AppProjects
`templates.v2.stakater.com/deletion-policy: Orphan`; Template Operator v0.0.7+
releases them when the tenant is opted out, deleted, or the chart is
uninstalled. Purge is opt-in, globally or per tenant.

## Consequences

- No accidental loss of a project and its Applications.
- On Template Operator v0.0.6 and earlier, AppProjects are deleted regardless;
  documented in the README and `docs/rollback.md`.
