# Diagrams

## C4 — Container (today)

```mermaid
flowchart LR
  subgraph Cluster[OpenShift / Kubernetes cluster]
    MTO[Multi-Tenant Operator<br/>Tenant, namespaces, IntegrationConfig]
    subgraph TOP[Template Operator v2]
      PTI[Parent TemplateInstance<br/>mto-argocd-parent]
      CTI[Child TemplateInstances<br/>one per opted-in tenant]
    end
    CH[[mto-extension-argocd<br/>Helm chart]]
    subgraph AN[Argo CD namespace]
      AP[AppProject per tenant]
      AC[Argo CD<br/>server, controller]
    end
    NS[Tenant namespaces]
  end
  IDP[Identity provider]
  CH --> PTI
  MTO -->|Tenants, Namespaces, IntegrationConfig| PTI
  PTI --> CTI -->|renders| AP
  AC -->|enforces| AP
  AC -->|syncs into| NS
  IDP -->|groups| AC
```

## C4 — Container, with the opt-ins on

```mermaid
flowchart LR
  subgraph Cluster[OpenShift / Kubernetes cluster]
    subgraph AN[Argo CD namespace, platform only]
      AP[AppProject acme<br/>sourceNamespaces, destinationServiceAccounts]
      AC[Argo CD]
    end
    subgraph SN[acme-argocd]
      APP[Applications and ApplicationSets<br/>created by the tenant]
    end
    subgraph DN[acme-dev, acme-prod]
      SA[ServiceAccount argocd-deployer<br/>RoleBinding to edit]
      W[Workloads]
    end
  end
  APP -->|project acme| AP
  AC -->|reads| APP
  AC -->|impersonates| SA -->|creates| W
```

## Sequence — onboarding a tenant (today)

```mermaid
sequenceDiagram
  autonumber
  participant P as Platform team
  participant T as Template Operator v2
  participant A as Argo CD
  participant U as Tenant owner
  P->>P: annotate Tenant acme: argocd...enabled=true
  T->>T: parent emits child for acme
  T->>A: AppProject acme (destinations, sourceRepos, roles)
  U->>A: sign in; group matches acme-owner
  U->>A: create Application in project acme → acme-dev
  A->>A: destination allowed; sync
```

## Sequence — a sync with impersonation (opt-in)

```mermaid
sequenceDiagram
  autonumber
  participant U as Tenant member
  participant A as Argo CD
  participant K as Kubernetes API
  U->>A: Application in acme-argocd, project acme, dest acme-dev
  A->>A: sourceNamespaces allows acme-argocd
  A->>K: apply as system:serviceaccount:acme-dev:argocd-deployer
  K-->>A: allowed for Deployments; refused for ClusterRoles
```
