# Vendored CRDs

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
