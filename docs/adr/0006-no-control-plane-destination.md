# ADR-0006: The Argo CD namespace is not a tenant destination

- **Status:** Accepted (2026-10-12) — **security, priority**
- **Date:** 2026-10-09
- **Revises:** ADR-0002 (destinations)

## Context

Today every tenant's AppProject lists the Argo CD namespace as a destination,
"even when it matches a privileged pattern", on the grounds that "ArgoCD
itself needs it to manage the project" (`child-gotemplate.tpl:95-104`). That
reason doesn't hold: the AppProject lives there regardless; destinations only
limit where Applications deploy. An Application in the control-plane namespace
may reference any AppProject, and anyone who can write there "is to be
considered an Argo CD admin" (N3). The controller that syncs is cluster-admin
(S4). So an owner or editor can sync an `Application`, an `AppProject` or a
repository/cluster Secret into the control plane.

## Decision

- The Argo CD namespace is a destination only with
  `argocd.allowControlPlaneDestination: true` (default false), and never when it
  matches a privileged pattern.
- Tenants who need Applications outside the control plane get them through
  `sourceNamespaces` (ADR-0007), not app-of-apps in the control plane.
- The equivalence test records this as a deliberate difference; release note.

## Consequences

- Closes the escalation path (AC-ISO-3); A.0 reproduces it first.
- Tenants using app-of-apps into the Argo CD namespace must move to
  `sourceNamespaces`; that's the migration note.

## Alternatives

- **Keep it, blacklist `argoproj.io/*` and `Secret`.** Rejected as default: the
  blacklist applies to all the tenant's namespaces and is easy to get wrong.
