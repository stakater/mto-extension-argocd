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
CHART_VERSION := $(shell awk '/^version:/{print $$2; exit}' $(CHART_DIR)/Chart.yaml)

HELM ?= $(LOCALBIN)/helm
HELM_VERSION ?= v3.16.3

# Where helm-release publishes. Same registry as the other extension charts.
HELM_REGISTRY ?= ghcr.io/stakater/charts

# Registry credentials. GHCR_* is what push.yaml sets; GIT_* is the local
# override so a developer can push to their own registry without exporting
# GHCR-shaped names. Exported so recipes can read them as $$SHELL variables --
# referencing $(GIT_TOKEN) in a recipe would print the token under `make -n`,
# which expands recipes even for @-prefixed lines.
GIT_USER  ?= $(GHCR_USERNAME)
GIT_TOKEN ?= $(GHCR_TOKEN)
export GIT_USER GIT_TOKEN

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): | $(LOCALBIN)
	@# Fetched to a file, then run -- never `curl | bash` (the cluster target's
	@# comment explains why). And pinned to a commit, not main: this used to
	@# pipe whatever helm/helm's main branch carried on the day CI ran. The SHA
	@# is the commit tag v3.16.3 peels to, resolved 2026-08-21; the script only
	@# downloads the release named by DESIRED_VERSION, so it need not move in
	@# step with HELM_VERSION.
	curl -fsSL -o $(LOCALBIN)/get-helm-3 \
		https://raw.githubusercontent.com/helm/helm/cfd07493f46efc9debd9cc1b02a0961186df7fdf/scripts/get-helm-3
	HELM_INSTALL_DIR=$(LOCALBIN) USE_SUDO=false DESIRED_VERSION=$(HELM_VERSION) \
		bash $(LOCALBIN)/get-helm-3

.PHONY: helm-lint
helm-lint: helm ## Lint the chart. argocd.namespace is required, so lint has to supply one.
	$(HELM) lint $(CHART_DIR) --strict --set argocd.namespace=lint-only

.PHONY: helm-render-test
helm-render-test: helm ## Render tests: execute both gotemplates against fixtures and diff the AppProjects.
	HELM=$(HELM) go test -count=1 ./tests/render/...

.PHONY: helm-render-test-regenerate
helm-render-test-regenerate: helm ## Rewrite the golden files. Review the diff before committing.
	HELM=$(HELM) REGENERATE=1 go test -count=1 ./tests/render/...

.PHONY: vet
vet: ## Vet and format-check the test harness.
	go vet ./...
	@# go vet does not check formatting, so an unformatted file used to reach
	@# main and show up as reformat noise in the next diff that touched it.
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:" >&2; echo "$$unformatted" >&2; \
		echo "run: gofmt -w ." >&2; exit 1; \
	fi

.PHONY: test
# CI calls this target directly rather than re-listing its prerequisites, so
# adding one here is enough to make it run on every pull request.
test: helm-lint vet helm-render-test admission-test ## Everything CI runs.

.PHONY: helm-login
helm-login: helm ## Log in to the OCI chart registry.
	@if [ -z "$$GIT_USER" ] || [ -z "$$GIT_TOKEN" ]; then \
		echo "GIT_USER/GIT_TOKEN (or GHCR_USERNAME/GHCR_TOKEN) must be set"; exit 1; \
	fi
	@echo "$$GIT_TOKEN" | $(HELM) registry login $(firstword $(subst /, ,$(HELM_REGISTRY))) -u "$$GIT_USER" --password-stdin

.PHONY: helm-release
# helm push has no existence check and no --force, and GHCR permits tag
# overwrite, so a republished version silently replaces what clusters pull.
#
# Fails closed: only an explicit not-found means the tag is free. helm show
# chart also exits 1 on DNS failure and an expired token, and treating those
# as free would publish over a released version on a network blip. Must run
# after helm-login -- unauthenticated, GHCR answers 403 for absent and
# published tags alike.
helm-release: helm helm-login helm-lint ## Package and push the chart. Refuses a version that is already published.
	@rc=0; out=$$($(HELM) show chart oci://$(HELM_REGISTRY)/mto-extension-argocd --version $(CHART_VERSION) 2>&1) || rc=$$?; \
	if [ $$rc -eq 0 ]; then \
		echo "mto-extension-argocd $(CHART_VERSION) is already published -- bump Chart.yaml before releasing"; exit 1; \
	fi; \
	if ! printf '%s' "$$out" | grep -qiE 'not found|manifest unknown|name unknown'; then \
		echo "cannot determine whether mto-extension-argocd $(CHART_VERSION) is published -- refusing to push:"; \
		printf '%s\n' "$$out" | head -3; exit 1; \
	fi
	@mkdir -p dist
	$(HELM) package $(CHART_DIR) -d dist
	$(HELM) push dist/mto-extension-argocd-$(CHART_VERSION).tgz oci://$(HELM_REGISTRY)

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
e2e-test: helm ## Run the E2E suite against a live cluster. Requires KUBE_CONTEXT, E2E_TENANT, E2E_ARGO_NAMESPACE.
	@# HELM is set here for the same reason every other suite sets it. This
	@# target used to omit it, so the e2e suite -- the only one that installs
	@# into a live cluster -- rendered the chart with whatever helm was on PATH
	@# while CI and every other suite used the pinned version.
	E2E=1 HELM=$(HELM) go test ./tests/e2e/... -v -timeout 20m -count=1

# ---------------------------------------------------------------- E2E cluster
#
# `make e2e-kind` builds the whole stack from nothing: a kind cluster,
# cert-manager, MTO's CRDs, ArgoCD, template-operator-v2, then the suite --
# which installs and uninstalls this chart itself, per test. Everything is
# idempotent, so re-running after a failure reuses what is already up.
#
# The namespaces below are the CHART DEFAULTS on purpose -- operatorNamespace
# and integrationConfig.namespace both default to multi-tenant-operator, and
# rbac.serviceAccount.namespace to stakater-template-operator. The kind install
# therefore passes only argocd.namespace, which has no default, so the E2E run
# exercises the values a customer gets rather than a bespoke set.

CONTAINER_TOOL ?= docker
KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KIND_VERSION ?= v0.30.0
KIND_CONFIG ?= hack/e2e/kind-config.yaml
TEST_CLUSTER_NAME ?= mto-argocd-e2e

# Not `kubectl config use-context`: the suite refuses to run against the
# ambient current-context, and every target here should be equally explicit.
E2E_CONTEXT ?= kind-$(TEST_CLUSTER_NAME)
KC = $(KUBECTL) --context $(E2E_CONTEXT)

E2E_OPERATOR_NS ?= multi-tenant-operator
E2E_TOV2_NS ?= stakater-template-operator
E2E_ARGO_NS ?= argocd

CERT_MANAGER_VERSION ?= v1.20.0
CERT_MANAGER_URL ?= https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
# The current ArgoCD line (#16): 3.5 is where sync impersonation is beta and
# ApplicationSets in any namespace are stable. OpenShift GitOps 1.22 ships
# 3.5.3. Override to check an older supported line, e.g. ARGOCD_VERSION=v3.4.10.
ARGOCD_VERSION ?= v3.5.4
ARGOCD_URL ?= https://raw.githubusercontent.com/argoproj/argo-cd/$(ARGOCD_VERSION)/manifests/install.yaml

# template-operator-v2's PUBLIC release. Chart and image are both anonymous
# pulls, so nothing here needs credentials -- neither locally nor in CI. The
# chart pins its own matching image tag, which is why only the chart version is
# named here.
#
# `ghcr.io/stakater/...` without the `public/` segment is the private release of
# the same operator and needs a `helm registry login`; TOV2_CHART and TOV2_IMG
# are here so you can point at it. TOV2_SRC is the third option: build a sibling
# checkout and load it into kind, which is the only way to test an unreleased
# operator against this chart.
TOV2_RELEASE ?= template-operator-v2
TOV2_CHART ?= oci://ghcr.io/stakater/public/charts/template-operator-v2
# TODO: once a template-operator-v2 version with the delete-retain fix is
# pinned here, set E2E_EXPECT_RETAIN=1 in the e2e-kind lane so the retain
# assertions in tests/e2e stop being report-only -- today they pass in every
# environment because no lane sets the flag.
TOV2_CHART_VERSION ?= 0.0.6
TOV2_IMG ?=
TOV2_SRC ?=

# `kubectl wait --for=condition=Established` on a CRD created moments earlier
# fails outright rather than waiting:
#
#   error: .status.conditions accessor error: <nil> is of the type <nil>,
#   expected []interface{}
#
# The object exists but has no status yet, and kubectl treats that as a fatal
# accessor error. Retrying is the documented workaround; three attempts is
# ample, since the gap is one reconcile of the apiextensions controller.
#
# The success flag is load-bearing: a bare for-loop's exit status is the last
# command's (the sleep), so exhausting the retries used to read as success and
# the failure surfaced later as a missing kind, blamed on whatever ran next.
define wait-crd
	@ok=""; for i in 1 2 3; do \
		$(KC) wait crd $(1) --for=condition=Established --timeout=60s && { ok=1; break; }; \
		echo "  CRDs not established yet, retrying ($$i/3)"; sleep 5; \
	done; \
	if [ -z "$$ok" ]; then echo "error: CRDs $(1) never became Established" >&2; exit 1; fi
endef

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): | $(LOCALBIN)
	@# -f, or an HTTP error page is saved as the kind "binary" with exit 0 and
	@# the failure surfaces later as "cannot execute binary file".
	curl -fsSLo $(KIND) "https://kind.sigs.k8s.io/dl/$(KIND_VERSION)/kind-$$(go env GOOS)-$$(go env GOARCH)"
	chmod +x $(KIND)

.PHONY: cluster
cluster: | $(LOCALBIN) ## Create the kind cluster (idempotent). Shared with every other Stakater operator repo.
	@# Fetched to a file, then run -- never `curl | bash`. Piping executes
	@# whatever partial or error output curl produced before failing, and -f
	@# keeps an HTTP error page from being executable at all.
	@# Pinned to a commit, not refs/heads/main, so a push to stakater/.github
	@# cannot change what this target executes. The SHA is stakater/.github's
	@# main as of 2026-08-21; bump it deliberately to pick up script changes.
	curl -fsSL -o $(LOCALBIN)/setup-kind-cluster.sh \
		https://raw.githubusercontent.com/stakater/.github/77c4bea83766c7dbf77fb3107df9efb9a07eb0e4/.github/scripts/setup-kind-cluster.sh
	TEST_CLUSTER_NAME=$(TEST_CLUSTER_NAME) \
	CONTAINER_TOOL=$(CONTAINER_TOOL) \
	KIND_VERSION=$(KIND_VERSION) \
	KIND_CONFIG=$(KIND_CONFIG) \
	LOCALBIN=$(LOCALBIN) \
	bash $(LOCALBIN)/setup-kind-cluster.sh cluster

.PHONY: cluster-delete
cluster-delete: kind ## Delete the kind cluster.
	$(KIND) delete cluster --name $(TEST_CLUSTER_NAME)

.PHONY: e2e-cert-manager
e2e-cert-manager: ## Install cert-manager. template-operator-v2's webhook certificate is issued through it.
	$(KC) apply -f $(CERT_MANAGER_URL)
	$(KC) wait deployment.apps/cert-manager-webhook --for=condition=Available -n cert-manager --timeout=300s
	$(call wait-crd,certificates.cert-manager.io issuers.cert-manager.io)
	@# Available=True is not the same as "the webhook answers". Issuing a
	@# throwaway Issuer is the only check that actually proves it, and without
	@# it the tov2 install races and fails with "connection refused".
	@# On exhaustion the target FAILS and re-runs the probe visibly: the loop
	@# discards the error text on every attempt, and a silent exit 0 here would
	@# surface two targets later as the exact "connection refused" race this
	@# probe exists to prevent.
	@ok=""; for i in $$(seq 1 30); do \
		echo '{"apiVersion":"cert-manager.io/v1","kind":"Issuer","metadata":{"name":"cm-readiness-probe","namespace":"cert-manager"},"spec":{"selfSigned":{}}}' | $(KC) apply -f - >/dev/null 2>&1 && \
		$(KC) delete issuer cm-readiness-probe -n cert-manager >/dev/null 2>&1 && { ok=1; break; }; \
		echo "  cert-manager webhook not ready yet ($$i/30)"; sleep 4; \
	done; \
	if [ -z "$$ok" ]; then \
		echo "error: cert-manager webhook never became ready after 30 probes; the last attempt said:" >&2; \
		echo '{"apiVersion":"cert-manager.io/v1","kind":"Issuer","metadata":{"name":"cm-readiness-probe","namespace":"cert-manager"},"spec":{"selfSigned":{}}}' | $(KC) apply -f - 1>&2 || true; \
		$(KC) delete issuer cm-readiness-probe -n cert-manager >/dev/null 2>&1 || true; \
		exit 1; \
	fi

.PHONY: e2e-mto
e2e-mto: ## Install MTO's CRDs and the IntegrationConfig. NOT the operator -- see hack/e2e/README.md.
	$(KC) apply -f tests/crds/tenantoperator.stakater.com_tenants.yaml \
		-f tests/crds/tenantoperator.stakater.com_integrationconfigs.yaml
	$(call wait-crd,tenants.tenantoperator.stakater.com integrationconfigs.tenantoperator.stakater.com)
	$(KC) create namespace $(E2E_OPERATOR_NS) --dry-run=client -o yaml | $(KC) apply -f -
	$(KC) apply -f hack/e2e/integrationconfig.yaml

.PHONY: e2e-argocd
e2e-argocd: ## Install ArgoCD. Supplies the AppProject CRD, and proves ArgoCD honours what the chart renders.
	$(KC) create namespace $(E2E_ARGO_NS) --dry-run=client -o yaml | $(KC) apply -f -
	@# Server-side: the upstream manifest carries CRDs whose annotations exceed
	@# the 256KB client-side apply limit.
	$(KC) apply -n $(E2E_ARGO_NS) --server-side -f $(ARGOCD_URL)
	$(call wait-crd,appprojects.argoproj.io applications.argoproj.io)
	@# Applications in any namespace (#9), so TestSourceNamespaces can show
	@# ArgoCD enforcing the rendered sourceNamespaces. The controller reads this
	@# only at start, so it is restarted -- but only when the value changed.
	@if ! $(KC) -n $(E2E_ARGO_NS) patch configmap argocd-cmd-params-cm --type merge \
		-p '{"data":{"application.namespaces":"*-argocd"}}' | grep -q 'no change'; then \
		$(KC) -n $(E2E_ARGO_NS) rollout restart statefulset/argocd-application-controller; \
	fi
	@# Only the three components the suite actually uses. Waiting on dex,
	@# notifications and applicationset would add pulls and flakiness for
	@# nothing.
	$(KC) -n $(E2E_ARGO_NS) rollout status deploy/argocd-repo-server --timeout=300s
	$(KC) -n $(E2E_ARGO_NS) rollout status deploy/argocd-server --timeout=300s
	$(KC) -n $(E2E_ARGO_NS) rollout status statefulset/argocd-application-controller --timeout=300s

.PHONY: e2e-tov2
e2e-tov2: helm kind ## Install template-operator-v2. Set TOV2_SRC=<checkout> to build from source instead of pulling.
	$(KC) create namespace $(E2E_TOV2_NS) --dry-run=client -o yaml | $(KC) apply -f -
	@# A release left in pending-install by an interrupted run blocks every later
	@# upgrade with "another operation is in progress", and there is nothing in it
	@# worth keeping.
	@if $(HELM) status $(TOV2_RELEASE) --kube-context $(E2E_CONTEXT) -n $(E2E_TOV2_NS) -o json 2>/dev/null | grep -q '"status":"pending'; then \
		echo "clearing a $(TOV2_RELEASE) release stuck mid-operation"; \
		$(HELM) uninstall $(TOV2_RELEASE) --kube-context $(E2E_CONTEXT) -n $(E2E_TOV2_NS); \
	fi
ifneq ($(TOV2_SRC),)
	@# From-source path. The image tag is fixed rather than derived from the
	@# checkout's git state: kind caches by tag, and a moving tag would make
	@# "did my rebuild land?" unanswerable.
	$(CONTAINER_TOOL) build -t template-operator-v2:e2e-local $(TOV2_SRC)
	$(KIND) load docker-image template-operator-v2:e2e-local --name $(TEST_CLUSTER_NAME)
	$(HELM) upgrade --install $(TOV2_RELEASE) $(TOV2_SRC)/charts/template-operator-v2 \
		--kube-context $(E2E_CONTEXT) -n $(E2E_TOV2_NS) --reset-values \
		--set controllerManager.manager.image.repository=template-operator-v2 \
		--set controllerManager.manager.image.tag=e2e-local
else
	@# Registry path -- the default, and it takes no credentials. The image is
	@# left at the chart's own default so the operator and its chart stay a
	@# matched pair; TOV2_IMG overrides that when you deliberately want them
	@# mismatched.
	@# --reset-values is not decoration. `helm upgrade` REUSES the previous
	@# release's user-supplied values when the command passes none of its own,
	@# so switching from TOV2_SRC back to the registry kept the locally built
	@# image while reporting the released chart -- and the version-dependent
	@# assertions in tests/e2e then measured the wrong operator. Measured, on
	@# this repo, after a chart-only upgrade left image tag e2e-local in place.
	$(HELM) upgrade --install $(TOV2_RELEASE) $(TOV2_CHART) \
		$(if $(TOV2_CHART_VERSION),--version $(TOV2_CHART_VERSION),) \
		--kube-context $(E2E_CONTEXT) -n $(E2E_TOV2_NS) --reset-values \
		$(if $(TOV2_IMG),--set controllerManager.manager.image.repository=$(firstword $(subst :, ,$(TOV2_IMG))) --set controllerManager.manager.image.tag=$(lastword $(subst :, ,$(TOV2_IMG))),)
endif
	@# BEFORE waiting for the rollout, and that order is not negotiable: an
	@# unscoped sync-enforcement webhook deadlocks the cluster hard enough that
	@# the rollout can never finish. The script explains the mechanism.
	KUBECTL=$(KUBECTL) E2E_CONTEXT=$(E2E_CONTEXT) ./hack/e2e/scope-tov2-webhook.sh
	$(KC) -n $(E2E_TOV2_NS) rollout status deploy/template-operator-v2-controller-manager --timeout=300s

.PHONY: e2e-deps
e2e-deps: e2e-cert-manager e2e-mto e2e-argocd e2e-tov2 ## Everything the chart needs, onto an existing cluster.

.PHONY: e2e-up
e2e-up: cluster e2e-deps ## Cluster plus dependencies. Leaves it running for repeated e2e-test runs.

.PHONY: e2e-kind
e2e-kind: ## Build the whole stack in kind and run the suite. The one command to remember.
	@# Recursive rather than prerequisites so a failure ANYWHERE in the chain --
	@# the cluster build, a dependency install, or the suite -- still dumps
	@# diagnostics before exiting. When e2e-up was a prerequisite, only suite
	@# failures produced ./e2e-logs, and the dependency-phase failures CI most
	@# needs diagnosed uploaded nothing.
	$(MAKE) e2e-up && $(MAKE) e2e-test \
		KUBE_CONTEXT=$(E2E_CONTEXT) \
		E2E_MANAGE_TENANTS=1 \
		E2E_ARGO_NAMESPACE=$(E2E_ARGO_NS) \
		E2E_OPERATOR_NAMESPACE=$(E2E_OPERATOR_NS) \
		E2E_TOV2_SA=system:serviceaccount:$(E2E_TOV2_NS):$(TOV2_RELEASE) \
		|| { $(MAKE) e2e-diagnostics; exit 1; }

.PHONY: e2e-diagnostics
e2e-diagnostics: ## Dump cluster state into ./e2e-logs. CI uploads this on failure.
	mkdir -p e2e-logs
	-$(KC) -n $(E2E_TOV2_NS) logs deploy/template-operator-v2-controller-manager --tail=6000 > e2e-logs/tov2.log 2>&1
	-$(KC) -n $(E2E_TOV2_NS) logs deploy/template-operator-v2-controller-manager --previous --tail=3000 > e2e-logs/tov2-previous.log 2>&1
	-$(KC) get templates.templates.v2.stakater.com,templateinstances.templates.v2.stakater.com -A -o yaml > e2e-logs/template-objects.yaml 2>&1
	-$(KC) get appprojects.argoproj.io,applications.argoproj.io -A -o yaml > e2e-logs/argocd-objects.yaml 2>&1
	-$(KC) get tenants.tenantoperator.stakater.com,integrationconfigs.tenantoperator.stakater.com -A -o yaml > e2e-logs/mto-objects.yaml 2>&1
	-$(KC) get ns --show-labels > e2e-logs/namespaces.txt 2>&1
	-$(KC) get pods -A -o wide > e2e-logs/pods.txt 2>&1
	-$(KC) get events -A --sort-by=.lastTimestamp > e2e-logs/events.txt 2>&1
