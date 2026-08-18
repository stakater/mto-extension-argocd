# Rollback commands

The decision — which option, and what it costs — is in
[MIGRATION.md](../MIGRATION.md#rolling-back). This file is only the commands.

`snapshot` first, every time. It writes `restore/<name>.json` per `AppProject` —
applyable as-is, tokens included — which is what these procedures fall back to.

## Option 2 — hand one tenant back with no downtime **[unverified]**

Detach the `AppProject` from the chart *before* removing the opt-in, so the reap
has nothing to reap. The object is never deleted and its UID never changes.

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

# 4. hand it to the controller, which adopts the existing object
kubectl apply -f extensions/acme.yaml

# 5. put strict back
helm upgrade mto-extension-argocd ./charts/mto-extension-argocd \
  --namespace multi-tenant-operator --set sync.mode=strict --reuse-values
```

## Option 3 — delete and recreate one tenant

**Order matters — re-apply the CR first.** The other order leaves the
`AppProject` deleted with nothing running to recreate it until you apply the CR.

```bash
# 1. put the Extensions CR back. It error-loops against the strict webhook; that
#    is expected, and being already retrying is what makes step 2 self-heal.
kubectl apply -f extensions/acme.yaml

# 2. remove the opt-in. The chart reaps the child and DELETES the AppProject.
kubectl annotate tenant acme argocd.tenantoperator.stakater.com/enabled-
```

## The whole release, before the CRD is removed

**Never `helm uninstall` on its own.** It deletes the RBAC and the
`TemplateInstance`s together, and each `TemplateInstance`'s finalizer needs that
RBAC to delete its `AppProject`. Teardown deadlocks and every object needs its
finalizer cleared by hand ([TOV2-5](upstream-issues.md)).

```bash
# 1. re-apply every Extensions CR you deleted
kubectl apply -f extensions/

# 2. drop the parent. Children and their AppProjects drain while the RBAC still
#    exists -- this DELETES every AppProject the chart manages, and the
#    controller then recreates each one.
kubectl delete templateinstances.templates.v2.stakater.com \
  mto-argocd-parent -n multi-tenant-operator

# 3. wait until no mto-argocd-tenant-* children remain, then
helm uninstall mto-extension-argocd -n multi-tenant-operator

# 4. clear the opt-in markers. kubectl reports "not found" for tenants that
#    never had one; that is noise, not a failure.
kubectl annotate tenant --all argocd.tenantoperator.stakater.com/enabled-
```

Then confirm with `diff ./before-migration` that you are back where you started.
