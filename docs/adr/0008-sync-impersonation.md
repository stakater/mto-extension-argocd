# ADR-0008: Sync impersonation, opt-in, one deployer per tenant namespace

- **Status:** Accepted (2026-10-12)
- **Date:** 2026-10-09

## Context

Without impersonation every sync runs as the application controller, which is
cluster-admin (S4). Impersonation maps each destination to a ServiceAccount;
beta in 3.5, also covering server operations (S1, S3); system-wide switch
(S2).

## Decision

- `argocd.impersonation.enabled` (default false) renders
  `destinationServiceAccounts`: one entry per tenant namespace,
  `defaultServiceAccount: argocd-deployer`; no catch-all.
- The ServiceAccount and its RoleBinding to `edit` come from a Multi-Tenant
  Operator template in every tenant namespace.
- Requires Argo CD 3.5+; documented that real damage limitation also needs a
  reduced controller ClusterRole.

## Consequences

- A sync can only do what the tenant could do with `kubectl` (AC-SS-2).
- `enforced: "false"` allows a gradual rollout.
- On OpenShift GitOps through `extraConfig`; Red Hat support to confirm (A.0).

## Alternatives

- **A catch-all ServiceAccount.** Rejected: silently widens access.
