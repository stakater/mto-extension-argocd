# Vision

## The problem

Platform teams run one Argo CD for many application teams. Argo CD's tenant
unit is the `AppProject`: which repositories a team may deploy from, which
namespaces it may deploy into, what it may create, and who may do what. Nothing
creates it from the platform's idea of a team. So the platform team writes
each AppProject by hand and edits it whenever the team gains a namespace,
a repository or a person.

The Multi-Tenant Operator used to do this with its `Extensions` CR and the
`ExtensionsArgoCDReconciler`. That integration is legacy and being removed;
this chart replaces it (README, "Differences from the Extensions controller").

| Pattern | What goes wrong |
|---|---|
| One shared project | every team can deploy anywhere, from any repository |
| AppProjects by hand | Friday's namespace isn't a destination; last quarter's team can still deploy |
| Applications only in the Argo CD namespace | tenants need write access to the control plane, which is Argo CD admin |

## The vision

> **One tenant definition, carried into Argo CD.** Every tenant gets its own
> AppProject — its namespaces as destinations, its repositories, roles from its
> groups — and, on current Argo CD, its own namespace for Applications and
> syncs that run with the tenant's own permissions. Derived from the `Tenant`,
> with no per-tenant setup.

## Who it is for

| Persona | Job to be done | What they get |
|---|---|---|
| **Platform team** | run one Argo CD for many teams | an AppProject per tenant, kept in step with namespaces and repositories; drift refused |
| **Application team** | deploy its own services by GitOps | self-service Applications inside its own boundary, without tickets |
| **Security team** | keep Argo CD's power away from tenants | no tenant path into the control plane; syncs limited to the tenant's own permissions |
| **Argo CD owners** | keep Argo CD the standard | AppProjects, Applications in any namespace and sync impersonation used as designed |

## Value proposition

- **Isolation by construction.** Destinations come from the tenant's namespaces,
  minus privileged ones; nothing is configured by hand.
- **Self-service without admin.** Tenants create Applications in their own
  namespace (target), never in the control plane.
- **Least privilege at sync time.** With impersonation, a sync can only do what
  the tenant's own ServiceAccount can (target).
- **Safe migration.** It replaces the Extensions controller one tenant at a
  time, without disrupting an AppProject.

## Principles

1. Argo CD enforces the boundary; the chart only writes the AppProject.
1. No tenant path into the Argo CD control plane.
1. Removing management never deletes a live AppProject by default.
1. When Argo CD can't do something per tenant, say so.

## Non-goals

- Installing or configuring Argo CD itself (OpenShift GitOps, the Argo CD
  operator or Helm).
- Per-tenant Argo CD instances.
- Multi-cluster tenants (Argo CD Agent) for now.

## How we'll know it works

- Every opted-in tenant has exactly one AppProject whose destinations equal its
  non-privileged namespaces.
- No tenant can deploy into the Argo CD namespace or another tenant's
  namespaces (acceptance suite).
- Migration from the Extensions controller loses no AppProject.
