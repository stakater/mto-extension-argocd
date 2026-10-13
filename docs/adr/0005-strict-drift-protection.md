# ADR-0005: Strict drift protection, with project tokens exempt

- **Status:** Accepted (built, PR #1)
- **Date:** 2026-08-06

## Decision

`sync.mode: strict` (default): Template Operator's admission webhook refuses
hand edits and deletes of managed AppProjects. `spec.roles[].jwtTokens` is
exempt so `argocd proj role create-token` works; tokens stay valid through
`status.jwtTokensByRole`.

## Consequences

- No silent drift; the looser `revert` can wedge a deleted AppProject in
  `Terminating`.
