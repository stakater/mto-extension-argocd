# Design

**Status:** Phase 1 runs today (README); Phase 2 in PR #2; Phases 3–5 are
proposed (every story Draft). Facts cited to `argocd-baseline.md`; decisions to
ADRs.

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
| `destinations` | the tenant's namespaces minus privileged ones, **plus the Argo CD namespace** (ADR-0006 proposes removing it) |
| `clusterResourceWhitelist` / `namespaceResourceBlacklist` | values or annotation |
| `roles` | `owner` and `edit` with `*`, `view` with `get`, over the six project resources incl. `exec`, `repositories`, `clusters` (ADR-0009 proposes narrowing) |
| role `groups` | the MTO-generated OpenShift group, privileged groups, the tenant's groups, then its users (ADR-0003) |

Drift: `sync.mode: strict` refuses hand edits (ADR-0005). Removal: released,
not deleted, on Template Operator v0.0.7+ (ADR-0004).

## 2. Findings against Argo CD 3.5 (2026-10)

| Priority | Finding | Fix | ADR / story |
|---|---|---|---|
| **P0** | The Argo CD namespace is a destination for every tenant. Through Argo CD's cluster-admin controller, an owner or editor can sync an `Application` (which in the control-plane namespace may reference any project), an `AppProject`, or a repository/cluster Secret there: escalation to Argo CD admin (N3). Also bypasses the privileged-namespace exclusion. | Remove it; opt-in only, never when privileged | ADR-0006, A.3 |
| **P0** | `exec` for owner and edit runs as the argocd-server ServiceAccount, outside the tenant's RBAC and audit (R2). | Off by default; owner only when enabled | ADR-0009, A.4 |
| **P0** | `clusters` write lets tenants register clusters (T1). | Off by default | ADR-0009, A.4 |
| **P0** | Owner and edit are identical (R3). | Edit without Application create, delete, override | ADR-0009, A.4 |
| P1 | Tenants can only create Applications in the control-plane namespace, which is admin (N3). | `sourceNamespaces: [<tenant>-argocd]` | ADR-0007, A.5 |
| P1 | Syncs run as the controller, cluster-admin (S4). | `destinationServiceAccounts`, opt-in, 3.5+ | ADR-0008, A.7 |
| P1 | Access only from `accessControl`; other extensions use group patterns. | Optional group pattern | ADR-0010, A.8 |
| P1 | Verified on 3.4.6; current is 3.5.4 / GitOps 1.22. | Re-verify, version gates | A.9 |

## 3. Target AppProject

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata: { name: acme, namespace: openshift-gitops }
spec:
  sourceRepos: [https://github.com/acme-org/acme-gitops]
  sourceNamespaces: [acme-argocd]                       # ADR-0007
  destinations:                                         # no control plane, ADR-0006
    - { server: https://kubernetes.default.svc, namespace: acme-dev }
    - { server: https://kubernetes.default.svc, namespace: acme-prod }
  destinationServiceAccounts:                           # ADR-0008, opt-in, 3.5+
    - { server: https://kubernetes.default.svc, namespace: acme-dev,  defaultServiceAccount: argocd-deployer }
    - { server: https://kubernetes.default.svc, namespace: acme-prod, defaultServiceAccount: argocd-deployer }
  roles:                                                # ADR-0009
    - name: acme-owner
      policies:
        - p, proj:acme:acme-owner, applications, *, acme/*, allow
        - p, proj:acme:acme-owner, applicationsets, *, acme/*, allow
        - p, proj:acme:acme-owner, logs, get, acme/*, allow
        - p, proj:acme:acme-owner, repositories, *, acme/*, allow
    - name: acme-edit
      policies:
        - p, proj:acme:acme-edit, applications, get, acme/*, allow
        - p, proj:acme:acme-edit, applications, sync, acme/*, allow
        - p, proj:acme:acme-edit, applications, update, acme/*, allow
        - p, proj:acme:acme-edit, applications, update/*, acme/*, allow
        - p, proj:acme:acme-edit, applications, action/*, acme/*, allow
        - p, proj:acme:acme-edit, logs, get, acme/*, allow
        - p, proj:acme:acme-edit, repositories, get, acme/*, allow
    - name: acme-view
      policies:
        - p, proj:acme:acme-view, applications, get, acme/*, allow
        - p, proj:acme:acme-view, logs, get, acme/*, allow
        - p, proj:acme:acme-view, repositories, get, acme/*, allow
```

The `<tenant>-argocd` namespace and the `argocd-deployer` ServiceAccounts come
from Multi-Tenant Operator templates (namespace `withTenantPrefix`, a
TemplateGroupInstance for the ServiceAccount and its RoleBinding to `edit`),
not from this chart, which only writes AppProjects.

## 4. New values (proposed)

```yaml
argocd:
  allowControlPlaneDestination: false     # ADR-0006
  sourceNamespaces:
    enabled: false                         # ADR-0007
    pattern: "{{ tenant }}-argocd"
  impersonation:
    enabled: false                         # ADR-0008, Argo CD 3.5+
    serviceAccount: argocd-deployer
roles:
  exec: { enabled: false }                 # ADR-0009
  clusters: { enabled: false }
identity:
  groupPattern: ""                         # e.g. "tenant-{tenant}-{role}s"; ADR-0010
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
