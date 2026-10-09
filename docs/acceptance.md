# Acceptance scenarios

Levels: **R** = render test (`tests/render`), **A** = admission test
(`tests/admission`), **E** = e2e (`tests/e2e`), **L** = live Argo CD 3.5 /
OpenShift GitOps 1.22. Status: ✅ covered today, 📝 to add.

Fixture: tenants `acme` (`acme-dev`, `acme-prod`) and `team-b` (`team-b-dev`);
privileged namespace pattern `openshift`; Argo CD in `openshift-gitops`.

## Isolation

**AC-ISO-1 — Destinations are the tenant's non-privileged namespaces.** *(R, E)* ✅

**AC-ISO-2 — Another tenant's namespace is refused.** *(L)* 📝 An Application in
`acme` deploying to `team-b-dev` is refused by Argo CD.

**AC-ISO-3 — No tenant path into the control plane.** *(R, L)* 📝 No rendered
AppProject lists the Argo CD namespace as a destination (unless explicitly
opted in, ADR-0006); on a live install, an `acme` Application whose source
contains an `AppProject`, `Application` or repository Secret for
`openshift-gitops` fails to sync.

**AC-ISO-4 — Source repositories are the tenant's.** *(R)* ✅ `sourceRepos` come
from per-tenant patterns only.

**AC-ISO-5 — Malformed input fails one tenant, never the fleet.** *(R)* ✅

## Access

**AC-ACC-1 — Roles map to the tenant's owners, editors and viewers.** *(R, E)* ✅

**AC-ACC-2 — Edit is narrower than owner.** *(R, L)* 📝 `acme-edit` can sync and
update, but not create, delete or override Applications (ADR-0009).

**AC-ACC-3 — No exec by default.** *(R)* 📝 No role has `exec` unless
`roles.exec.enabled` (ADR-0009).

**AC-ACC-4 — No cluster registration by tenants.** *(R)* 📝 No role has
`clusters` write unless opted in (ADR-0009).

**AC-ACC-5 — Group-pattern identity, optionally.** *(R)* 📝 With
`identity.groupPattern` set, roles carry `tenant-acme-owners` etc. (ADR-0010).

## Self-service (target)

**AC-SS-1 — Applications in the tenant's own namespace.** *(R, L)* 📝 With
`sourceNamespaces` on, an `acme` member creates an Application in
`acme-argocd`; an Application there referencing project `team-b` is refused.

**AC-SS-2 — Syncs run as the tenant.** *(L)* 📝 With impersonation on, an `acme`
Application whose manifests create a ClusterRole fails to sync, because
`argocd-deployer` in `acme-dev` can't create one.

## Lifecycle

**AC-LIFE-1 — Scaffolding modes pick tenants.** *(R)* ✅

**AC-LIFE-2 — A new namespace becomes a destination.** *(E)* ✅

**AC-LIFE-3 — Removing management releases, never deletes, by default.**
*(E, Template Operator v0.0.7+)* ✅

**AC-LIFE-4 — Drift is refused.** *(E)* ✅ With `sync.mode: strict`, a hand edit or
delete of a managed AppProject is refused.

**AC-LIFE-5 — Project tokens survive renders.** *(E)* ✅

## Migration

**AC-MIG-1 — Equivalent to the Extensions controller, except the documented
differences.** *(R)* ✅ `TestEquivalenceWithExtensionsController`.

**AC-MIG-2 — Cut-over loses no AppProject.** *(R, E)* 🔨 PR #2.
