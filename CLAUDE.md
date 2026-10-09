# CLAUDE.md

Guidance for working in this repository.

## What this is

**The Argo CD extension for the Multi-Tenant Operator.** A Helm chart,
rendered by Template Operator v2, that gives every opted-in `Tenant` its own
Argo CD `AppProject`. It replaces `ExtensionsArgoCDReconciler` and the
`Extensions` CRD. The README is the user documentation; `docs/` holds the
requirements, design and decisions.

## Status

Phase 1 runs today (PR #1); migration from the Extensions controller is PR #2.
Phases 3–5 are proposed: every story there is 📝 Draft until a human approves.
**Phase 3 (A.3, A.4) is security work and goes first**, after the A.0 live
check.

## Invariants

1. One AppProject per tenant; destinations list the tenant's non-privileged
   namespaces explicitly (ADR-0002).
1. **No tenant path into the Argo CD control plane** — the target of ADR-0006;
   don't add new ones.
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
| `docs/adr/` | Decisions: 0001–0005 built, 0006–0010 proposed. |
| `docs/e2e.md`, `docs/rollback.md` | Existing operational docs. |

## Workflow

Story → planning doc (`docs/planning/TEMPLATE.md`) → red test → change →
`make test` green, docs updated → PR citing the story.

## Boundaries

- Argo CD baseline 3.5 (verified today on 3.4.6); OpenShift GitOps 1.22.
- The customer deck lives in `stakater-sales/mto-sales`
  (`src/pages/argocd.astro`, `SPEAKER-NOTES-ARGOCD.md`) and follows this repo.
