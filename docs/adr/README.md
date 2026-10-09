# Architecture Decision Records

Immutable once accepted; supersede, don't edit. Facts cite
`../argocd-baseline.md`.

| # | Decision | Status |
|---|---|---|
| [0001](0001-chart-rendered-by-template-operator.md) | A chart rendered by Template Operator v2, no controller | Accepted |
| [0002](0002-one-appproject-per-tenant.md) | One AppProject per tenant, destinations listed explicitly | Accepted |
| [0003](0003-roles-from-tenant-access-control.md) | Roles from the Tenant's access lists, for equivalence | Accepted |
| [0004](0004-release-not-delete.md) | Removing management releases the AppProject by default | Accepted |
| [0005](0005-strict-drift-protection.md) | Strict drift protection, project tokens exempt | Accepted |
| [0006](0006-no-control-plane-destination.md) | The Argo CD namespace is not a tenant destination — **security** | Proposed |
| [0007](0007-applications-in-tenant-namespace.md) | Tenant Applications in a dedicated `<tenant>-argocd` namespace | Proposed |
| [0008](0008-sync-impersonation.md) | Sync impersonation, opt-in, one deployer per namespace | Proposed |
| [0009](0009-least-privilege-roles.md) | Least-privilege project roles — **security** | Proposed |
| [0010](0010-optional-group-pattern-identity.md) | Optional group-pattern identity | Proposed |
