# Running E2E

```bash
make e2e-kind
```

That builds a kind cluster, installs everything the chart needs, and runs the
suite — which installs and uninstalls the chart itself, per test — dumping
diagnostics into `./e2e-logs` if anything fails. It is idempotent: run it again
and it reuses the cluster.

## What you need

| | |
|---|---|
| docker | kind runs the cluster inside it |
| kubectl, go | on `PATH` |
| helm | downloaded into `bin/` if absent; pass `HELM=helm` to use your own |

**No credentials.** Every component is an anonymous pull, including
template-operator-v2, whose public release lives at
`oci://ghcr.io/stakater/public/charts/template-operator-v2` — pinned to `0.0.6`
by the Makefile, and it carries its own matching image tag.

Two other sources, when you want them:

```bash
# the private release of the same operator -- needs `helm registry login ghcr.io`
make e2e-kind TOV2_CHART=oci://ghcr.io/stakater/charts/template-operator-v2 \
              TOV2_CHART_VERSION=

# a sibling checkout: builds the image, loads it into kind, installs that
# checkout's chart. The only way to test an UNRELEASED operator against this
# chart -- which is how the retain behaviour was confirmed fixed.
make e2e-kind TOV2_SRC=../template-operator-v2
```

Everything else about the run is identical in all three.

## What it stands up

[hack/e2e/README.md](../hack/e2e/README.md) has the component list and the
reasoning, including why **multi-tenant-operator itself is not installed** —
only its CRDs. The short version: the chart never needs MTO to *run*, and MTO's
webhooks would remove the two capabilities this cluster exists to provide
(creating tenants freely, and creating a namespace that matches a privileged
pattern).

## Iterating

`make e2e-kind` re-runs the whole dependency chain each time, which is fast
once the images are cached but not instant. While working on the suite itself:

```bash
make e2e-up TOV2_SRC=../template-operator-v2   # once
make e2e-test \
  KUBE_CONTEXT=kind-mto-argocd-e2e \
  E2E_MANAGE_TENANTS=1 \
  E2E_ARGO_NAMESPACE=argocd \
  E2E_OPERATOR_NAMESPACE=multi-tenant-operator \
  E2E_TOV2_SA=system:serviceaccount:stakater-template-operator:template-operator-v2
```

`E2E_TOV2_SA` is required here: the chart's `rbac.serviceAccount` default is
the account MTO runs template-operator-v2 as, and this cluster runs a
standalone release.

Add `TEST_ARGS='-run TestManagedFleet'` style filtering by editing the target,
or call `go test` directly with the same environment — `make e2e-test` is a
thin wrapper over it.

Tear down with `make cluster-delete`.

## Running against a real cluster instead

The suite has a second mode that borrows an existing tenant on a licensed
cluster, which is what proves the chart works *alongside* MTO.

The suite installs its own release, `mto-argocd-e2e`. The chart supports one
release per cluster, so first uninstall any existing release and delete the
RBAC it keeps (the cleanup commands that NOTES.txt prints).

`E2E_OPERATOR_NAMESPACE` is where the suite installs the chart, and also where
it looks for the IntegrationConfig. It defaults to `E2E_ARGO_NAMESPACE`, so set
it to the namespace that holds the IntegrationConfig, usually
`multi-tenant-operator`:

```bash
E2E=1 \
KUBE_CONTEXT="<your-kube-context>" \
E2E_TENANT=<an-existing-tenant> \
E2E_ARGO_NAMESPACE=<argocd-namespace> \
E2E_OPERATOR_NAMESPACE=<integrationconfig-namespace> \
E2E_TOV2_SA=system:serviceaccount:multi-tenant-operator:tenant-operator-template-operator-v2 \
make e2e-test
```

`E2E_TOV2_SA` is the account template-operator-v2 runs as. The value above is
the one MTO deploys it with; a standalone release runs as an account named
after the release, in the release namespace. The suite binds the chart's RBAC
to it and probes the RBAC as it.

The package comment in [tests/e2e](../tests/e2e/e2e_test.go) spells out the
difference. Tests that need to create tenants or namespaces skip there, and say
why.

## CI

[.github/workflows/e2e.yaml](../.github/workflows/e2e.yaml) runs the same
`make e2e-kind` on a GitHub runner, with kind created **on the runner itself** —
there is no shared cluster, and no secrets. It is not part of every pull
request: an approval from a repo owner, org member or collaborator, the
`ok-to-test/kubernetes` label, or a manual dispatch triggers it — an approval
from anyone else deliberately does not, because the run executes PR-authored
code. `e2e-logs/` is uploaded as an artifact when it fails.

The fast chart tests — lint, vet, render, admission — do run on every pull
request, through `make test`. They catch most regressions in seconds; this
suite exists for the ones that only appear when template-operator-v2 actually
reconciles.

## What the suite covers

Nine test functions, thirty-odd assertions, ~4.5 minutes. Grouped by what each
one is actually for:

| Area | Where | What it pins |
|---|---|---|
| Lifecycle | `TestLifecycle` | install → opt in → break an annotation → fix it → identical upgrade → descaffold → purge → uninstall, in dependency order against one release |
| Drift protection | `TestLifecycle` | `strict` refuses an out-of-band DELETE **and** an out-of-band edit, while still allowing the `spec.roles[*].jwtTokens` write that `argocd proj role create-token` makes — checked against the live webhook rather than the rendered value. The write goes through the Kubernetes API, so ArgoCD's own project validation is covered by the render tests, not here |
| Fleet behaviour | `TestManagedFleet` | privileged namespaces never become destinations, a tenant with no namespaces still gets a usable project, a namespace event re-renders exactly one tenant, a tenant nobody opted in gets nothing, role group order |
| Scaffolding modes | `TestScaffoldingModes`, `TestScaffoldingModeNarrowing` | `OnLabel` reads the label and ignores the annotation; `Never` stops rendering but keeps the reap verbs, or every child finalizer deadlocks; narrowing a mode releases or deletes, and which one it does is reported |
| Live object semantics | `TestLiveObjectSemantics` | an AppProject that already exists is adopted **in place** (same UID — migration's load-bearing claim), a namespace moves cleanly between tenants, the ArgoCD namespace is listed once, an explicit `[]` strips the field from the live object, the sourceRepos pattern expands per tenant, a values change updates in place |
| Blast radius | `TestFailuresStayContained` | a malformed annotation fails one tenant and moves no other object; an unreadable or missing IntegrationConfig fails the parent and leaves the fleet running; deleting a Tenant reaps only its own child; the parent's roll-up names the broken tenant |
| ArgoCD itself | `TestArgoCDHonoursTheRenderedProject` | ArgoCD refuses an Application outside the rendered destinations and accepts one inside them |
| Source namespaces | `TestSourceNamespaces` | a tenant's source namespace is listed only while it exists with the tenant's label: absent until created, never when it belongs to another tenant, removed from the live object when relabelled; ArgoCD runs an Application there under the tenant's own project and refuses one under another tenant's. `make e2e-argocd` sets `application.namespaces: "*-argocd"` for this; the ArgoCD subtests skip on a cluster without it |
| RBAC | `TestLeastPrivilegeDelta` | the chart grants exactly five permissions and no others, measured as a before/after delta |
| Upstream characterization | `TestRevertModeWedgesOnDelete` | the `revert` wedge, reported either way so a fix upstream is visible |

**What is deliberately NOT here.** Chart values validation — a blank
`argocd.namespace`, a bogus `scaffolding.mode`, `strict` without its exemptions,
a bad sourceRepos placeholder, YAML injection through tenant strings, the
privileged filter failing closed — all live in `tests/render`, which executes
the same gotemplates through template-operator-v2's own function map in
milliseconds. Repeating them here would cost minutes and prove less.

Two assertions are **version-dependent** and report rather than fail: whether
descaffolding retains or deletes an AppProject, and whether `revert` still
wedges. Both are properties of the operator build underneath, not of this chart,
and the run says which one it saw. Against an operator that honours the retain
annotation (v0.0.7+, e.g. `TOV2_CHART_VERSION=0.0.7`), set `E2E_EXPECT_RETAIN=1` to turn
the retain report into an assertion — otherwise a chart regression in release
semantics could hide behind "not implemented in this build".

## What this cluster cannot tell you

Worth knowing before treating a green run as "shipped":

- **Nothing here runs multi-tenant-operator.** Its webhooks admitting what the
  chart writes, and its generated group names matching what the roles assume,
  are covered only by a run against a real cluster; see
  [Running against a real cluster instead](#running-against-a-real-cluster-instead).
- **Nobody logs in.** The three roles and their policies are rendered and
  accepted by ArgoCD, but no member of those groups has been authenticated
  against them. That needs SSO and ingress.
- **One node, no OpenShift.** `syncOCPGroups`, Routes, and SCC behaviour are
  out of reach by construction.
- **Overlong tenant names.** `mto-argocd-tenant-<name>` exceeds the 253-char
  object-name limit for tenant names ≥ 236 chars. The parent renders such a
  child; what happens on-cluster — whether the one invalid child stays
  contained or destabilizes the parent apply — is untested. TODO: a
  `TestFailuresStayContained` case with a ≥ 236-char tenant name, once a lane
  exists where creating one is cheap.
