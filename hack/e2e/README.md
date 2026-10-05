# E2E cluster assets

What `make e2e-up` puts on a kind cluster, and why each piece is there. The
chart under test is deliberately NOT on this list: the suite installs and
uninstalls it itself, per test, during `make e2e-test`.

| Component | How | Why |
|---|---|---|
| kind cluster | [kind-config.yaml](kind-config.yaml) | Kubernetes 1.33 — tov2's CRDs carry a CEL rule 1.31 cannot compile |
| cert-manager | upstream manifest | template-operator-v2's chart issues its webhook certificate through a cert-manager `Issuer`/`Certificate` |
| MTO CRDs | [tests/crds](../../tests/crds) | `Tenant` and `IntegrationConfig` are the parent template's inputs |
| IntegrationConfig | [integrationconfig.yaml](integrationconfig.yaml) | supplies `accessControl.privileged`, without which the parent refuses to render |
| ArgoCD | upstream `install.yaml`, pinned | provides the `AppProject` CRD, and lets the suite prove ArgoCD honours what the chart renders |
| template-operator-v2 | its Helm chart | the thing that actually reconciles. Nothing in this repo runs; the chart is the deliverable |

## multi-tenant-operator itself is deliberately NOT installed

Only its CRDs are. Three reasons, in order of weight:

1. **The chart never needs MTO to run.** It reads `Tenant` and
   `IntegrationConfig` objects through template-operator-v2's `objectFieldRef`
   parameters. Whether a controller reconciles those objects is irrelevant to
   every assertion in the suite.
2. **MTO's webhooks would take capability away.** Its namespace webhook refuses
   any namespace without a `stakater.com/tenant` label, and the free tier caps
   a cluster at two Tenants. That combination is exactly why the live-cluster
   E2E has to *borrow* a tenant instead of creating one — and why the
   privileged-namespace path was untestable there. On this cluster the
   suite owns its tenants and namespaces, so that path is now covered.
3. **MTO is a private image behind a licence.** Requiring it would put a
   credential in the way of `make e2e-kind` for anyone outside Stakater.

The cost is real and worth naming: nothing here proves the chart works
*alongside* MTO — that MTO's own webhooks admit what the chart writes, that its
generated groups are named what the roles assume. That needs a run against a
real OpenShift cluster with MTO; see
[Running against a real cluster instead](../../docs/e2e.md#running-against-a-real-cluster-instead).
