# Design

**Status:** Phases 1, 3, 4 and 5 are built (chart 0.7.1, 2026-10-13); Phase 2,
the migration, is PR #2; the A.0 live check is open (#8). Facts cited to
`argocd-baseline.md`; decisions to ADRs.

## 1. What runs today

A Helm chart, no controller (ADR-0001). Three Template Operator v2 objects:

| Object | Role |
|---|---|
| `Template/mto-argocd-parent` | ranges over every Tenant, emits one child per tenant that passes the scaffolding filter |
| `Template/mto-argocd-child` | renders one tenant's AppProject |
| `TemplateInstance/mto-argocd-parent` | the only TemplateInstance in git |

Inputs: a cluster-wide list of Tenants and Namespaces, and the
IntegrationConfig (`accessControl.privileged` only). Three watches regardless
of tenant count.

Per tenant, AppProject `<tenant>` in `argocd.namespace` (ADR-0002):

| Field | Today |
|---|---|
| `sourceRepos` | per-tenant patterns, `{{ tenant }}` expanded; annotation override |
| `destinations` | the tenant's namespaces minus privileged ones; never the Argo CD namespace (ADR-0006) unless `argocd.allowControlPlaneDestination`, for migration |
| `sourceNamespaces` | opt-in: `<tenant>-argocd`, listed only while it exists with the tenant's label (ADR-0007) |
| `destinationServiceAccounts` | opt-in: `argocd-deployer` in each tenant namespace, no catch-all (ADR-0008) |
| `clusterResourceWhitelist` / `namespaceResourceBlacklist` | values or annotation; a whitelist `name` only with `argocd.allowClusterResourceNames` (Argo CD 3.3+) |
| `syncWindows` / `orphanedResources` | values or annotation, off by default |
| `roles` | least privilege (ADR-0009): owner without `exec` and `clusters` write unless enabled; edit syncs and updates but cannot create, delete or override; view reads. `roles.profile: extensionsCompatible` is the legacy shape, for migration |
| role `groups` | the MTO-generated OpenShift group, privileged groups, then the tenant's groups and users (ADR-0003), a pattern group (ADR-0010), or both |

Drift: `sync.mode: strict` refuses hand edits (ADR-0005). Removal: released,
not deleted, on Template Operator v0.0.7+ (ADR-0004).

## 2. Findings against Argo CD 3.5 (2026-10-09), all fixed

The review of chart 0.1.0 that led to ADR-0006..0010. Kept as the record of
why the chart looks as it does.

| Priority | Finding | Fix | ADR / story | Fixed in |
|---|---|---|---|---|
| **P0** | The Argo CD namespace was a destination for every tenant. Through Argo CD's cluster-admin controller, an owner or editor could sync an `Application` (which in the control-plane namespace may reference any project), an `AppProject`, or a repository/cluster Secret there: escalation to Argo CD admin (N3). It also bypassed the privileged-namespace exclusion. | Removed; legacy shape opt-in, for migration only | ADR-0006, A.3 | #17, 0.2.0 |
| **P0** | `exec` for owner and edit ran as the argocd-server ServiceAccount, outside the tenant's RBAC and audit (R2). | Off by default; owner only when enabled | ADR-0009, A.4 | #17, 0.2.0 |
| **P0** | `clusters` write let tenants register clusters (T1). | Off by default | ADR-0009, A.4 | #17, 0.2.0 |
| **P0** | Owner and edit were identical (R3). | Edit without Application create, delete, override | ADR-0009, A.4 | #17, 0.2.0 |
| P1 | Tenants could only create Applications in the control-plane namespace, which is admin (N3). | `sourceNamespaces: [<tenant>-argocd]` | ADR-0007, A.5 | #18, 0.3.0 |
| P1 | Syncs run as the controller, cluster-admin (S4). | `destinationServiceAccounts`, opt-in, 3.5+ | ADR-0008, A.7 | #19, 0.4.0 |
| P1 | Access only from `accessControl`; other extensions use group patterns. | Optional group pattern | ADR-0010, A.8 | #20, 0.5.0 |
| P1 | Verified on 3.4.6; current is 3.5.4 / GitOps 1.22. | e2e on 3.5.4, version gates | A.9 | #23, 0.7.1 |

## 3. The AppProject with every option on

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata: { name: acme, namespace: openshift-gitops }
spec:
  sourceRepos: [https://github.com/acme-org/acme-gitops]
  sourceNamespaces: [acme-argocd]                       # ADR-0007, opt-in
  destinations:                                         # no control plane, ADR-0006
    - { server: https://kubernetes.default.svc, namespace: acme-argocd }
    - { server: https://kubernetes.default.svc, namespace: acme-dev }
    - { server: https://kubernetes.default.svc, namespace: acme-prod }
  destinationServiceAccounts:                           # ADR-0008, opt-in, 3.5+
    - { server: https://kubernetes.default.svc, namespace: acme-argocd, defaultServiceAccount: argocd-deployer }
    - { server: https://kubernetes.default.svc, namespace: acme-dev,    defaultServiceAccount: argocd-deployer }
    - { server: https://kubernetes.default.svc, namespace: acme-prod,   defaultServiceAccount: argocd-deployer }
  roles:                                                # ADR-0009, leastPrivilege
    - name: acme-owner
      policies:
        - p, proj:acme:acme-owner, applications, *, acme/*, allow
        - p, proj:acme:acme-owner, applicationsets, *, acme/*, allow
        - p, proj:acme:acme-owner, logs, get, acme/*, allow
        - p, proj:acme:acme-owner, repositories, *, acme/*, allow
        - p, proj:acme:acme-owner, clusters, get, acme/*, allow
    - name: acme-edit
      policies:
        - p, proj:acme:acme-edit, applications, get, acme/*, allow
        - p, proj:acme:acme-edit, applications, sync, acme/*, allow
        - p, proj:acme:acme-edit, applications, update, acme/*, allow
        - p, proj:acme:acme-edit, applications, update/*, acme/*, allow
        - p, proj:acme:acme-edit, applications, action/*, acme/*, allow
        - p, proj:acme:acme-edit, applicationsets, get, acme/*, allow
        - p, proj:acme:acme-edit, logs, get, acme/*, allow
        - p, proj:acme:acme-edit, repositories, get, acme/*, allow
        - p, proj:acme:acme-edit, clusters, get, acme/*, allow
    - name: acme-view
      policies:
        - p, proj:acme:acme-view, applications, get, acme/*, allow
        - p, proj:acme:acme-view, applicationsets, get, acme/*, allow
        - p, proj:acme:acme-view, logs, get, acme/*, allow
        - p, proj:acme:acme-view, repositories, get, acme/*, allow
        - p, proj:acme:acme-view, clusters, get, acme/*, allow
```

The `<tenant>-argocd` namespace and the `argocd-deployer` ServiceAccounts come
from Multi-Tenant Operator templates (namespace `withTenantPrefix`, a
TemplateGroupInstance for the ServiceAccount and its RoleBinding to `edit`),
not from this chart, which only writes AppProjects. The namespace carries the
tenant label, so it is also a destination.

## 4. Values

```yaml
argocd:
  allowControlPlaneDestination: false     # ADR-0006; true only for migration
  sourceNamespaces:
    enabled: false                        # ADR-0007
    pattern: "{{ tenant }}-argocd"        # must contain {{ tenant }}, no globs
  impersonation:
    enabled: false                        # ADR-0008, Argo CD 3.5+
    serviceAccount: argocd-deployer
  allowClusterResourceNames: false        # whitelist `name`, Argo CD 3.3+
  syncWindows: []
  orphanedResources: "off"                # off | warn | monitor
roles:
  profile: leastPrivilege                 # ADR-0009; extensionsCompatible for migration
  exec: { enabled: false }
  clusters: { enabled: false }
identity:
  mode: accessControl                     # ADR-0010; groupPattern | both
  groupPattern: "tenant-{{ tenant }}-{{ tier }}"
```

Annotation overrides per tenant follow the existing `argocd.tenantoperator.stakater.com/` convention.

## 5. Platform prerequisites (documented, not set by the chart)

- `application.namespaces` (or `ArgoCD.spec.sourceNamespaces`) covering
  `*-argocd`; on 3.5 / GitOps 1.22, ApplicationSet namespaces and SCM
  hardening (N2, N8–N10).
- `application.sync.impersonation.enabled` and a reduced controller
  ClusterRole for real damage limitation (S2, S4, S5).
- `policy.default` empty (R4); `scopes` including the user-name claim if users
  are listed (I1); no local users named like tenant groups (I2).

## 6. What Argo CD can't do per tenant — say so

| Wanted | Why not | Instead |
|---|---|---|
| sync impersonation per tenant only | it is a system-wide switch (S2) | enable for all; non-tenant projects map to their own accounts |
| real limits without trimming the controller | the controller stays cluster-admin (S4) | document the reduced ClusterRole |
| Applications in any namespace on namespace-scoped instances | needs a cluster-scoped Argo CD (N2) | the default cluster-scoped instance |
