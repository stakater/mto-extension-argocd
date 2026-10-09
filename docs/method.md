# Method — story → plan → red → green → verify → validate

How this extension is built. Adapted from the KubeWard method
(`stakater-ab/kube-ward` `docs/method.md`), itself a customised BMAD: brief and
PRD ↔ `vision.md` and `roadmap.md`, architect ↔ ADRs, story files ↔ story +
planning-doc pairs, QA gate ↔ `make` and acceptance scenarios. Same method as
`mto-extension-kasten`, `mto-extension-rhacs` and `mto-extension-quay`.

## The loop

```mermaid
flowchart LR
  V[Vision and value<br/>vision.md] --> S[Story<br/>roadmap.md]
  S -->|approval gate| P[Planning doc<br/>planning/ from TEMPLATE.md]
  P --> R[Red<br/>failing render test or acceptance check]
  R --> G[Green<br/>change the chart]
  G --> VER[Verify<br/>make, docs updated, PR]
  VER --> VAL[Validate<br/>acceptance scenarios on a live Argo CD]
  VAL -->|findings become stories| S
  ADR[ADRs, immutable] -.->|constrain| P
  RB[argocd-baseline.md] -.->|facts| P
```

The **inner loop** ships one story. The **outer loop** validates on a live
Argo CD against `acceptance.md` — the isolation contract is only proven there,
not by green render tests.

## Practices and where each is enforced

| Practice | Here | Enforced by |
|---|---|---|
| Spec first | vision → story → planning doc before chart or operator changes | workflow in CLAUDE.md; planning doc committed first |
| Immutable decisions | `docs/adr/`, superseded never edited | review |
| Story-driven | house format, Done line, acceptance ids, lifecycle | roadmap; PRs cite the story |
| Test first | a failing render test (golden, schema, guard) or acceptance check before the change | `make test`; review |
| Facts before design | Argo CD behaviour cited to `argocd-baseline.md` rows; unknowns marked VERIFY | review; A.0 |
| Live validation | acceptance scenarios on Argo CD 3.5 | validation report per phase |
| Honesty | "not verified" beats a confident guess | review |

## Story lifecycle

📝 Draft → 👍 Approved → 🔨 In progress → ✅ Done. A Draft must never be built.
Killing a weak Draft is the method working.

## The planning doc is a context capsule

`planning/TEMPLATE.md` is the contract: a cold session, with no memory of this
conversation, must be able to execute the story from the planning doc alone —
the why, the governing ADRs and baseline rows, the exact files, the tests (red
first), and what is out of scope.

## When Argo CD changes

1. Update `argocd-baseline.md` (version, changed rows, new VERIFY items).
1. Re-run the acceptance suite on the new version.
1. Supersede any ADR whose context changed.
