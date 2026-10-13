# Argo CD baseline — what Argo CD gives tenancy, and where it stops

**Versions (2026-10-09):** Argo CD **v3.5.4** (2026-10-06) is the latest GA;
v3.4.10 and v3.3.15 were patched the same day; v3.6.0-rc2 is in pre-release.
Upstream supports the last three minors (3.5, 3.4, 3.3). Red Hat **OpenShift
GitOps 1.22.0** (2026-09-30) ships Argo CD 3.5.3; 1.21 ships 3.4.3; 1.20 ships
3.3.2. The chart was verified on Argo CD 3.4.6 (README); since chart 0.7.1 the
e2e suite runs on 3.5.4.

> **Written against chart 0.1.0** (2026-10-09). Where a consequence below says
> "today", it describes 0.1.0; the roadmap (A.3–A.12, charts 0.2.0–0.7.1)
> records what was built.

Every decision cites a row. **VERIFY** marks what isn't settled; each is a task
in story A.0. Sources: `argo-cd.readthedocs.io/en/stable/…`,
`github.com/argoproj/argo-cd` (releases, `pkg/apis/application/v1alpha1`,
manifests), `docs.redhat.com/…/red_hat_openshift_gitops/1.22/…`,
`github.com/argoproj-labs/argocd-operator`.

## 1. Versions

| # | Fact | Consequence | Source |
|---|---|---|---|
| V1 | v3.5.4 latest GA; 3.6.0-rc2 pre-release. | Re-verify on 3.5.x (A.9): e2e on 3.5.4 since 0.7.1. | releases v3.5.4, v3.6.0-rc2 |
| V2 | Last three minors supported. | 3.5, 3.4, 3.3 today. | `SECURITY.md` |
| V3 | 3.0: `update`/`delete` no longer cover managed resources (`update/*`, `delete/*`); `logs` RBAC always enforced; Dex users identified by `federated_claims.user_id`. | Viewers need `logs, get` (the chart grants it). | upgrading/2.14-3.0 |
| V4 | 3.0–3.2 stop the project API exposing credentials of project-scoped repositories and clusters. | Run 3.x+ before tenants self-serve repository credentials. | upgrading/3.1-3.2 |
| V5 | 3.2: `application.sync.requireOverridePrivilegeForRevisionSync` makes syncing another revision need `override`. | `applications, *` hands out `override`. | rbac/#the-override-action |
| V6 | 3.3: cluster-resource allow/deny lists can match by name (globs). | 0.1.0 dropped `name`; since 0.7.1 it is kept behind `argocd.allowClusterResourceNames` (A.12), because older versions drop it on apply. | user-guide/projects; v3.3.0 |
| V7 | 3.5: impersonation beta (also for server operations), `enforced` flag; ApplicationSets in any namespace stable; Source Integrity replaces `signatureKeys`. | 3.5 completes the current tenancy model. | v3.5.0; upgrading/3.4-3.5 |
| V8 | 3.6 (rc): opt-in separate `rollback` action. | Watch. | PR #28924 |
| V9 | OpenShift GitOps 1.22 → Argo CD 3.5.3 (OCP 4.18–4.22); 1.21 → 3.4.3; 1.20 → 3.3.2. | Impersonation beta and the 3.4.4 RBAC fix only on 1.22. | GitOps 1.22 release notes |

## 2. Applications and ApplicationSets in any namespace

| # | Fact | Consequence | Source |
|---|---|---|---|
| N1 | Applications in any namespace since 2.5; no maturity marker, not on the alpha/beta list: stable. | Safe to build on. | app-any-namespace; feature-maturity |
| N2 | Two gates: `application.namespaces` (server and controller) **and** `AppProject.spec.sourceNamespaces`. Globs and regex. Needs a cluster-scoped Argo CD and extra server RBAC. | The chart sets only the AppProject half; the rest is a prerequisite. | app-any-namespace |
| N3 | An Application in the **control-plane namespace may reference any AppProject**; whoever can write Kubernetes objects there "is to be considered an Argo CD admin". | Tenants must never be able to write there — including through Argo CD itself (ADR-0006). | app-any-namespace |
| N4 | RBAC objects become `<project>/<namespace>/<app>`; `proj/*` still matches all namespaces. | Existing `acme/*` policies keep working. | rbac/#application-in-any-namespaces |
| N5 | 3-segment policies were broken with apps in any namespace until 3.4.4 / 3.5.0. | 3-segment policies need 3.4.4+; GitOps 1.21 (3.4.3) lacks the fix. | issue #25289; PR #28062 |
| N6 | OpenShift GitOps: `ArgoCD.spec.sourceNamespaces` (globs, regex), GA since 1.13; the operator labels the namespace and creates a Role/RoleBinding. | Supported on the default cluster-scoped instance. | GitOps 1.22 "Argo CD applications" |
| N7 | argocd-operator: a namespace can't carry both `managed-by` and `managed-by-cluster-argocd`; Applications and app resources in the same source namespace need custom roles. | A dedicated `<tenant>-argocd` namespace for Applications (ADR-0007). | argocd-operator `apps-in-any-namespace.md` |
| N8 | ApplicationSets in any namespace: stable in 3.5; needs the same namespaces in `applicationsetcontroller.namespaces`; generated Applications land in the ApplicationSet's namespace. | Fits `<tenant>-argocd`. | Appset-Any-Namespace; PR #27353 |
| N9 | ApplicationSet risks: SCM and PR generators can leak secrets (mitigate with `allowed.scm.providers`, `enable.scm.providers: false`, `tokenref.strict.mode`); `clusters: {}` discovers every cluster. | Admin prerequisites, documented (A.6). | Appset-Any-Namespace |
| N10 | OpenShift GitOps 1.22: ApplicationSets in any namespace GA (`spec.applicationSet.sourceNamespaces`, `.scmProviders`). | Needs 1.22. | GitOps 1.22 release notes |

## 3. Sync impersonation

| # | Fact | Consequence | Source |
|---|---|---|---|
| S1 | Alpha in 2.13–3.4; **beta in 3.5.0**. | Gate A.7 on 3.5. | feature-maturity; PR #27576 |
| S2 | `application.sync.impersonation.enabled: "true"` (system-wide); `destinationServiceAccounts[]` `{server, namespace (glob), defaultServiceAccount}`, first match wins; no match fails the sync unless `enforced: "false"` (3.5). | One entry per tenant namespace; **no catch-all**. | app-sync-using-impersonation |
| S3 | Covers sync and finalizer deletion; from 3.5 also server operations (get, patch, delete, events, pod logs, actions). | The tenant ServiceAccount needs about namespace `edit`. | upgrading/3.4-3.5 |
| S4 | The controller ClusterRole ships as `*/*/*` ("clusteradmin level"). | Real damage limitation needs the controller role reduced too; otherwise impersonation enforces tenant boundaries at sync time only. | manifests …application-controller-clusterrole.yaml; security/#cluster-rbac |
| S5 | OpenShift GitOps: no dedicated field; `extraConfig`; the operator's e2e covers it; `defaultClusterScopedRoleDisabled` / `aggregatedClusterRoles` shrink the controller role. | Works on 1.22; Red Hat support statement **VERIFY**. | argocd-operator tests and types |

## 4. RBAC

| # | Fact | Consequence | Source |
|---|---|---|---|
| R1 | Project role policies may only use `applications, applicationsets, repositories, exec, logs, clusters`; `*` as a resource is rejected. | The chart's six-line shape is right. | `types.go` `validatePolicy` |
| R2 | `exec` has only `create`, needs `exec.enabled`, and runs as the argocd-server ServiceAccount — the user "effectively has the same privileges". | `exec` for owner/edit is a shell outside the tenant's own RBAC and audit; `exec, get` for view does nothing (ADR-0009). | web_based_terminal; rbac |
| R3 | `applications, *` includes `update/*`, `delete/*`, `override`, `action/*`, `sync`. | Owner and edit were identical in 0.1.0; narrowed in 0.2.0 (ADR-0009). | rbac |
| R4 | Default policies apply to every authenticated user and can't be denied; OpenShift GitOps's default instance gives non-admins nothing. | Isolation depends on `policy.default` staying empty. | rbac/#default-policy; GitOps access control |
| R5 | `signatureKeys` deprecated in 3.5 (use `sourceIntegrity`); nothing else deprecated; API still `argoproj.io/v1alpha1`. | The chart uses no deprecated fields. | `types.go`; upgrading/3.4-3.5 |

## 5. Other tenancy

| # | Fact | Consequence | Source |
|---|---|---|---|
| T1 | Project-scoped repositories and clusters are Secrets with a `project` field; self-service needs `repositories`/`clusters` create/update/delete. | Owners managing repository credentials is legitimate; tenants registering clusters rarely is. | user-guide/projects |
| T2 | `permitOnlyProjectScopedClusters` restricts destinations to the project's clusters. | Multi-cluster only. | project.yaml |
| T3 | `sourceRepos` and `destinations` support `!` negation and globs. | Keep explicit namespace lists. | user-guide/projects |
| T4 | Sync windows (`syncOverrun` in 3.5), `orphanedResources.warn`, global projects. | Per-tenant annotations since 0.6.0 (A.11), `syncOverrun` included. | project.yaml |
| T5 | Source Hydrator beta; Technology Preview in GitOps 1.22. | Out of scope. | feature-maturity |
| T6 | OpenShift GitOps: user-defined instances are namespace-scoped; `managed-by` gives namespace-admin; `openshift-gitops` is cluster-scoped. | **VERIFY** whether the default instance's controller writes into tenant namespaces without `managed-by` on 1.22. | GitOps "Argo CD instance" |
| T7 | Argo CD Agent 0.10.0 GA in GitOps 1.22. | Multi-cluster tenants later. | GitOps 1.22 release notes |

## 6. Identity

| # | Fact | Consequence | Source |
|---|---|---|---|
| I1 | Role `groups` become `g, <group>, proj:<p>:<role>`; matched against the user identifier, then the claims in argocd-rbac-cm `scopes` (default `[groups]`). | User entries need `scopes: [groups, email]` (README). | `server/rbacpolicy/rbacpolicy.go` |
| I2 | An SSO group named like a local user inherits that user's roles. | Prefixed, IdP-managed tenant groups (ADR-0010). | rbac "Ambiguous Group Assignments" |
| I3 | Direct OIDC loads groups with `requestedScopes` / `requestedIDTokenClaims`. | Groups without Dex. | user-management |
| I4 | OpenShift GitOps: Dex with `openShiftOAuth` passes OpenShift groups; with OCP external authentication (4.20+) use `spec.oidcConfig`. | The Multi-Tenant Operator's generated OpenShift group doesn't reach Argo CD under external OIDC. | GitOps access control |

## Open questions (VERIFY), owned by story A.0

1. Reproduce the control-plane escalation on chart 0.1.0: an `acme` owner
   syncs an `AppProject`/`Application`/repository Secret into the Argo CD
   namespace. (N3, ADR-0006)
1. Does the default `openshift-gitops` controller write into tenant namespaces
   without the `managed-by` label on 1.22? (T6)
1. Red Hat support for impersonation through `extraConfig` on GitOps 1.22. (S5)
1. Does exec through the web terminal honour impersonation in 3.5? (R2, S3)
1. Do the `managed-by` / custom-roles caveats still hold on 1.22? (N7)
1. Can a tenant with `clusters, create` register a project-scoped cluster that
   shadows `https://kubernetes.default.svc`? (T1)
1. Do project-scoped repositories bypass `sourceRepos`? (T1)
1. OpenShift GitOps lifecycle: which of 1.20–1.22 are supported.
