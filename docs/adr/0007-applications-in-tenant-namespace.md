# ADR-0007: Tenant Applications live in a dedicated `<tenant>-argocd` namespace

- **Status:** Accepted (2026-10-12)
- **Date:** 2026-10-09

## Context

Applications in any namespace is stable (N1) and needs two gates: the global
`application.namespaces` and the AppProject's `sourceNamespaces` (N2). On
OpenShift GitOps, a namespace can't be both `managed-by` and a source namespace
without custom roles (N7). ApplicationSets in any namespace are stable in 3.5
(N8).

## Decision

- `sourceNamespaces: [<pattern>]`, default pattern `{{ tenant }}-argocd`,
  opt-in (`argocd.sourceNamespaces.enabled`); never the control plane or a
  privileged namespace.
- The pattern must contain `{{ tenant }}` and expand to a plain namespace
  name. Argo CD reads `sourceNamespaces` as globs and regexes, so a shared
  value or a glob (`{{ tenant }}*` for `acme` matches `acme-corp-argocd`) lets
  one tenant's namespace use another tenant's AppProject.
- Listed only while the namespace exists and carries the tenant's label, like
  a destination: never a namespace that belongs to someone else.
- The namespace itself comes from the Multi-Tenant Operator (tenant namespace
  with prefix), not this chart.
- Global settings are prerequisites, documented.

## Consequences

- Self-service Applications without control-plane access.
- Existing `acme/*` policies already cover them (N4); 3-segment policies need
  3.4.4+ (N5).
- Kubernetes RBAC on the source namespace decides who creates Applications
  there (`kubectl apply`); the project roles govern only the Argo CD UI and
  API.

## Alternatives

- **All tenant namespaces as source namespaces.** Rejected: N7 conflicts and
  mixes Applications with workloads.
