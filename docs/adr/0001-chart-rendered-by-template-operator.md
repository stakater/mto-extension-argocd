# ADR-0001: A chart rendered by Template Operator v2, no controller

- **Status:** Accepted (built, PR #1)
- **Date:** 2026-08-06

## Context

The AppProject is a Kubernetes object Argo CD reconciles itself; nothing needs
an external API. The legacy `ExtensionsArgoCDReconciler` in the Multi-Tenant
Operator is being removed.

## Decision

A Helm chart whose parent Template ranges over every Tenant and emits one child
per tenant; Template Operator v2 renders and owns the AppProjects. No operator,
image or OLM bundle.

## Consequences

- Correctness lives in templates: render tests run the gotemplates through
  Template Operator's own function map, plus property, admission and e2e tests.
- Three watches regardless of tenant count.
- Behaviour on removal depends on the Template Operator version (ADR-0004).

## Alternatives

- **Keep the Go controller.** Rejected: legacy, being removed.
- **A new Go operator.** Rejected: templating does the job.
