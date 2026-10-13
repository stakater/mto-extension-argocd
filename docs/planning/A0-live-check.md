# A.0 — Live check on Argo CD 3.5 / OpenShift GitOps 1.22

Story: `docs/roadmap.md` A.0 (📝 Draft — needs 👍 Approved before starting).
Acceptance: AC-ISO-3 (reproduce, then prove fixed), AC-SS-1, AC-SS-2.

## Why (one paragraph)

The research behind ADR-0006..0010 is from docs and source, and the changes
shipped before this check (charts 0.2.0–0.7.1; the e2e suite runs on kind with
Argo CD 3.5.4). What only a live install can show is still open: that the
control-plane escalation was real on 0.1.0, and that the new features work on
OpenShift GitOps 1.22, where customers run them.

## Governing constraints

ADR-0006..0010 are Accepted and built (chart 0.2.0 to 0.7.1); A.0 confirms
them on live clusters or supersedes them. ADR-0001..0005 are built and stay. Record exact
versions. No credentials in findings.

## Design

Environments: kind with Argo CD 3.5.4 (`make e2e-kind`, version bumped), and
OpenShift with GitOps 1.22.

| # | Question | How | If the answer differs |
|---|---|---|---|
| 1 | Control-plane escalation with chart 0.1.0, and refused from 0.2.0 | as an `acme` owner, sync an Application whose source holds an `AppProject` and a repository Secret for the Argo CD namespace | if blocked, record why; ADR-0006 stays as defence in depth |
| 2 | Default `openshift-gitops` writes into tenant namespaces without `managed-by` | deploy to `acme-dev` unlabelled | document the label as a prerequisite, or impersonation |
| 3 | Impersonation via `extraConfig` on GitOps 1.22, and Red Hat's support position | enable, sync as `argocd-deployer` | gate A.7 on support |
| 4 | Exec honours impersonation in 3.5 | open a terminal with impersonation on | keep exec off regardless |
| 5 | `managed-by` / custom-roles caveats on 1.22 | create `acme-argocd` as source namespace | adjust ADR-0007 |
| 6 | Project-scoped cluster shadowing `kubernetes.default.svc` | owner with `clusters, create` | keep clusters off |
| 7 | Project-scoped repositories vs `sourceRepos` | create one outside the patterns | document |
| 8 | GitOps lifecycle | Red Hat lifecycle page | set the support matrix |

Output: `docs/spikes/a0-findings.md`.

## Tests (red first)

Question 1's reproduction becomes the failing e2e test for A.3.

## Out of scope

Changing the chart (A.3 onward).

## Done means

All answered with evidence; ADR-0006..0010 Accepted or superseded; baseline
updated; A.3 ready to start with a red test.
