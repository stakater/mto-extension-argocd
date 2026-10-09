# Concepts

One running example: tenant **`acme`**, namespaces **`acme-dev`**,
**`acme-prod`**, Argo CD in **`openshift-gitops`**.

## The terms

| Term | Where it lives | What it is |
|---|---|---|
| **Tenant** | Multi-Tenant Operator | A team and its namespaces, labelled `stakater.com/tenant=<tenant>`; owners, editors and viewers in `spec.accessControl`. |
| **IntegrationConfig** | Multi-Tenant Operator | Cluster settings; this chart reads only `accessControl.privileged` (namespaces excluded from every tenant, groups added to every role). |
| **AppProject** | Argo CD, its namespace | The tenant unit: `sourceRepos`, `destinations`, resource allow/deny lists, roles. One per opted-in tenant: `acme`. |
| **Destination** | AppProject | A namespace (on a cluster) an Application may deploy into. Today: the tenant's non-privileged namespaces, plus the Argo CD namespace (see ADR-0006). |
| **Project role** | AppProject | `acme-owner`, `acme-edit`, `acme-view`: policy lines over applications, applicationsets, logs, exec, repositories, clusters, and the groups that hold them. |
| **Source namespace** | AppProject `sourceNamespaces` | A namespace outside the control plane where tenants create their own Applications (target: `acme-argocd`). |
| **Sync impersonation** | AppProject `destinationServiceAccounts` | Argo CD syncs as a named ServiceAccount in the destination namespace, not as its own controller (target). |
| **Template Operator v2** | Stakater | Renders the parent Template over every Tenant and one child per tenant; the chart's only runtime. |
| **Scaffolding mode** | chart values | Which tenants get an AppProject: `OnAnnotation` (default), `OnLabel`, `Always`, `Never`. |

## The example, end to end (today)

1. The chart is installed with `argocd.namespace=openshift-gitops` and
   `sourceRepos.pattern: [https://github.com/acme-org/{{ tenant }}-gitops]`.
1. `acme` is annotated `argocd.tenantoperator.stakater.com/enabled: "true"`.
1. Template Operator renders AppProject `acme`: destinations `acme-dev`,
   `acme-prod` (and `openshift-gitops`), source repository
   `https://github.com/acme-org/acme-gitops`, three roles bound to the tenant's
   groups and users.
1. A member of `acme`'s owners group signs in to Argo CD and creates an
   Application in project `acme` deploying to `acme-dev`. Deploying to
   `team-b-dev` is refused by Argo CD.
1. `acme-test` is added; on the next render it becomes a destination.
1. The annotation is removed: the AppProject is released, not deleted
   (Template Operator v0.0.7+).

## The example (target)

1. `acme` also gets `sourceNamespaces: [acme-argocd]`: its Applications live
   in its own namespace, and the Argo CD namespace is no longer a destination.
1. With impersonation on, each destination maps to the ServiceAccount
   `argocd-deployer` in that namespace; a sync can do only what that account
   can.
