# mto-extension-argocd -- the deliverable is the Helm chart. There is no image,
# no OLM bundle and no operator here.
#
# The Go module exists solely for the render tests: template-operator-v2
# executes the chart's gotemplates with text/template + sprig, and tests/render
# mirrors that engine exactly. It depends only on public modules, so CI needs no
# private-module auth.

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LOCALBIN ?= $(shell pwd)/bin
CHART_DIR ?= charts/mto-extension-argocd

HELM ?= $(LOCALBIN)/helm
HELM_VERSION ?= v3.16.3

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): | $(LOCALBIN)
	curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
		| HELM_INSTALL_DIR=$(LOCALBIN) USE_SUDO=false DESIRED_VERSION=$(HELM_VERSION) bash

.PHONY: helm-lint
helm-lint: helm ## Lint the chart. argocd.namespace is required, so lint has to supply one.
	$(HELM) lint $(CHART_DIR) --strict --set argocd.namespace=lint-only

.PHONY: helm-render-test
helm-render-test: helm ## Render tests: execute both gotemplates against fixtures and diff the AppProjects.
	HELM=$(HELM) go test -count=1 ./tests/...

.PHONY: helm-render-test-regenerate
helm-render-test-regenerate: helm ## Rewrite the golden files. Review the diff before committing.
	HELM=$(HELM) REGENERATE=1 go test -count=1 ./tests/...

.PHONY: vet
vet: ## Vet the test harness.
	go vet ./...

.PHONY: test
test: helm-lint vet helm-render-test admission-test ## Everything CI runs.

ENVTEST ?= $(LOCALBIN)/setup-envtest
# 1.31 cannot compile template-operator-v2's CEL rule on objectFieldRef
# ("undefined field 'namespace'"), so the CRDs fail to install there. 1.33 matches
# OpenShift 4.20, the deployment target.
ENVTEST_K8S_VERSION ?= 1.33.0

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19

.PHONY: envtest-assets
envtest-assets: envtest ## Download the kube-apiserver/etcd binaries the admission tests need.
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

.PHONY: admission-test
admission-test: helm envtest ## Check the API server accepts what the chart renders (CRD schema + CEL).
	@# Resolve the assets FIRST and fail if empty. Inlining the $$(...) into the
	@# test invocation does not trip set -e, so a failed download would export an
	@# empty KUBEBUILDER_ASSETS, the suite would skip, and the run would be green
	@# with zero coverage.
	@assets="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)"; \
	if [ -z "$$assets" ]; then echo "envtest assets missing; run make envtest-assets" >&2; exit 1; fi; \
	REQUIRE_ADMISSION_TESTS=1 HELM=$(HELM) KUBEBUILDER_ASSETS="$$assets" go test -count=1 ./tests/admission/...

.PHONY: e2e-test
e2e-test: ## Run the E2E suite against a live cluster. Requires KUBE_CONTEXT, E2E_TENANT, E2E_ARGO_NAMESPACE.
	E2E=1 go test ./tests/e2e/... -v -timeout 20m -count=1
