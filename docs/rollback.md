# Rollback commands

Everything here covers chart-managed installs — AppProjects this chart created
or adopted. Migrating existing controller-managed AppProjects onto the chart,
and rolling such a migration back, ships with its own tooling and runbook in a
follow-up PR; those procedures are not in this file.

Snapshot first, every time. A full dump is applyable as-is, tokens included,
and is what these procedures fall back to:

```bash
kubectl get appprojects.argoproj.io -n <argocd-namespace> -o yaml \
  > appprojects-backup.yaml
```

## Release one tenant's AppProject without deleting it **[unverified]**

Detach the `AppProject` from the chart *before* removing the opt-in, so the reap
has nothing to reap. The object is never deleted and its UID never changes.

On template-operator-v2 `8a44194` and later, `onDeletePurgeAppProject: false` —
the chart default — already makes an opt-out release the AppProject rather than
delete it, and none of this is needed. On v0.0.6 and earlier it is deleted
regardless ([TOV2-1](upstream-issues.md)); the detach below is the workaround.

The pieces are measured — adoption, the reap, the strict webhook — but **this
sequence is not.** Treat it as an incident procedure and rehearse it on staging.

```bash
# 1. strict forbids editing a managed AppProject at all, including by you
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator --set sync.mode=revert --reuse-values

# 2. detach: drop template-operator-v2's finalizer and ownership labels
kubectl patch appproject acme -n <argocd-namespace> --type=merge -p \
  '{"metadata":{"finalizers":null,"labels":{"templates.v2.stakater.com/instance":null,"templates.v2.stakater.com/instance-name":null,"templates.v2.stakater.com/instance-namespace":null,"templates.v2.stakater.com/instance-uid":null}}}'

# 3. the reap now finds nothing it owns, and the AppProject survives
kubectl annotate tenant acme argocd.tenantoperator.stakater.com/enabled-

# 4. put strict back
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator --set sync.mode=strict --reuse-values
```

The AppProject is now unmanaged: nothing renders it and nothing reaps it.
Hand-manage it, or opt the tenant back in and the chart adopts it in place —
same UID, pinned by `tests/e2e`.

## The whole release

**Never `helm uninstall` on its own.** It deletes the RBAC and the
`TemplateInstance`s together, and each `TemplateInstance`'s finalizer needs that
RBAC to delete its `AppProject`. Teardown deadlocks and every object needs its
finalizer cleared by hand ([TOV2-5](upstream-issues.md)).

```bash
# 1. drop the parent. Children and their AppProjects drain while the RBAC still
#    exists. On template-operator-v2 8a44194+ the AppProjects are RELEASED and
#    survive; on v0.0.6 and earlier they are DELETED regardless (TOV2-1).
kubectl delete templateinstances.templates.v2.stakater.com \
  mto-argocd-parent -n multi-tenant-operator

# 2. wait until no mto-argocd-tenant-* children remain, then
helm uninstall mto-extension-argocd -n multi-tenant-operator

# 3. clear the opt-in markers. kubectl reports "not found" for tenants that
#    never had one; that is noise, not a failure.
kubectl annotate tenant --all argocd.tenantoperator.stakater.com/enabled-
```

Then diff what survived against the snapshot before declaring yourself back
where you started.
