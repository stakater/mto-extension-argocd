# Vendored CRDs

## template-operator-v2

`templates.v2.stakater.com_{templates,templateinstances}.yaml`, copied verbatim
from `template-operator-v2/config/crd/bases/` at **v0.0.6-3-g5fd8f5e**.

They are here so `tests/admission` can stand up an API server and check that
the objects this chart renders are actually *accepted* — the structural schema
plus the CEL rules on `spec.outputs`, `spec.gotemplate` and the parameter
lists. The render tests prove the gotemplates produce the right AppProject;
these prove the Template CRs carrying them will apply.

Vendored rather than read from a sibling checkout because CI has neither the
tov2 repository nor credentials for it — the module is private.

**Refresh them when tov2 changes its CRDs**, and record the new version here.
A stale copy silently narrows what the admission tests can catch.

## multi-tenant-operator

`tenantoperator.stakater.com_{tenants,integrationconfigs}.yaml`, from
`tenant-operator/helm-charts/tenant-operator/crds/` at
**v1.9.0-cf4a826e-4-g0ff789fa**, with two edits, both required:

- **`spec.conversion` removed.** It points at MTO's own webhook service, which
  does not exist on a cluster running the CRDs without the operator, so every
  read of a `Tenant` would fail. Dropping it leaves `strategy: None`; the E2E
  suite reads and writes each kind at its storage version — `Tenant` at
  `v1beta3`, `IntegrationConfig` at `v1beta1` — so no conversion is needed.
- **the `cert-manager.io/inject-ca-from` annotation removed**, for the same
  reason -- it names MTO's certificate.

They are here so `make e2e-kind` can stand up the chart's inputs -- `Tenant` and
`IntegrationConfig` -- without installing multi-tenant-operator, which is a
licensed private image whose webhooks would take away the two capabilities that
cluster exists to provide. [hack/e2e/README.md](../../hack/e2e/README.md)
explains that trade in full.

**Refresh them when MTO changes those CRDs**, re-applying both edits, and
record the new version here.
