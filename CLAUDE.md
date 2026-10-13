# CLAUDE.md

Guidance for working in this repository.

## What this is

**The Argo CD extension for the Multi-Tenant Operator.** A Helm chart,
rendered by Template Operator v2, that gives every opted-in `Tenant` its own
Argo CD `AppProject`. It replaces `ExtensionsArgoCDReconciler` and the
`Extensions` CRD. The README is the user documentation; `docs/` holds the
requirements, design and decisions.

## Status

Phases 1, 3, 4 and 5 are built (chart 0.7.1, 2026-10-13): stories A.1 and
A.3–A.12. Migration from the Extensions controller (A.2) is PR #2. The A.0 live
check on Argo CD 3.5 / OpenShift GitOps 1.22 is still 📝 Draft (#8). A new
story starts as 📝 Draft until a human approves it.

## Invariants

1. One AppProject per tenant; destinations list the tenant's non-privileged
   namespaces explicitly (ADR-0002).
1. **No tenant path into the Argo CD control plane** (ADR-0006): the Argo CD
   namespace is never a destination or a source namespace by default; don't
   add new paths.
1. A source namespace is listed only while the tenant owns it (ADR-0007).
1. Removing management releases, never deletes, by default (ADR-0004).
1. Malformed tenant input fails that tenant only; never fall back to a default
   that would strip a live field.
1. Render tests use Template Operator's own function map, not Helm's.

## Documentation map

| Doc | What it is |
|---|---|
| `README.md` | User documentation: install, configure, operate. |
| `docs/vision.md` | Why, for whom, value, non-goals. |
| `docs/concepts.md` | Terms and the running example. |
| `docs/roadmap.md` | Phases and stories (A.0–A.12). |
| `docs/acceptance.md` | Scenarios, with what tests cover today. |
| `docs/design.md` | What runs today, findings against Argo CD 3.5, the target. |
| `docs/argocd-baseline.md` | Argo CD 3.5 / OpenShift GitOps 1.22 facts with sources. |
| `docs/adr/` | Decisions: 0001–0010, all accepted and built. |
| `docs/e2e.md`, `docs/rollback.md` | Existing operational docs. |

## Workflow

Story → planning doc (`docs/planning/TEMPLATE.md`) → red test → change →
`make test` green, docs updated → PR citing the story.

## Boundaries

- Argo CD baseline 3.5: the e2e suite runs on 3.5.4; some README facts were
  verified by hand on 3.4.6. OpenShift GitOps 1.22.
- The customer deck lives in `stakater-sales/mto-sales`
  (`src/pages/argocd.astro`, `SPEAKER-NOTES-ARGOCD.md`) and follows this repo.
