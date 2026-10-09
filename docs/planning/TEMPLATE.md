# {Story code} — {short title}

<!--
The planning doc is a CONTEXT CAPSULE (method.md): a cold session with no
memory and no conversation history must be able to execute this story from this
file alone. Fill every section; write "none" rather than deleting one — an
explicit "none" is information, an absent section is a question. Delete this
comment block in the real doc.
-->

Story: `docs/roadmap.md` {Story code} (status must be 👍 Approved before this
doc is implemented). Acceptance: {AC ids}.

## Why (one paragraph)

{What is missing today, for whom, and why now.}

## Governing constraints

{ADRs and CLAUDE.md invariants by number, one clause each on how they bite
here. Argo CD facts by `argocd-baseline.md` row, e.g. "N3: the control-plane namespace is admin".}

## Design

{The change at file level: charts, templates, helpers, values, schema — exact
paths. Rendered objects shown as YAML snippets. What is NOT changing.}

## Tests (red first)

{Render tests (golden fixture, schema validation, guards, name budgets) and
live acceptance checks to write BEFORE the change, with the AC id each pins,
including negative cases.}

## Out of scope

{What this story deliberately doesn't do, and where that lives.}

## Done means

{The roadmap Done line, plus: `make` green, golden regenerated if intended,
Chart.yaml version bumped, docs updated (which), acceptance ids passing at
which level.}
