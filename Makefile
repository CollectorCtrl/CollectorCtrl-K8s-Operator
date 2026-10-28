# CollectorCtrl Kubernetes Operator
#
# Every target here works from a clean checkout with Go, Docker and Helm.

IMG ?= ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART := helm-charts/collectorctrl-operator
# Values needed to render the chart (the chart refuses to render without a credential source).
CHART_TEST_VALUES := --set opamp.enrollmentSecret=collectorctrl-auth
# Tool versions (CI uses the same).
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION ?= v1.9.0

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: verify build

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Format Go code.
	gofmt -w cmd internal operator pkg

.PHONY: fmt-check
fmt-check: ## Fail if Go code is not formatted.
	@out="$$(gofmt -l cmd internal operator pkg)"; if [ -n "$$out" ]; then echo "Unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: ## Run unit tests with the race detector and coverage.
	go test -race -coverprofile=cover.out ./...

.PHONY: license-check
license-check: ## Fail if a Go or shell file lacks the SPDX licence header.
	@missing="$$(git ls-files '*.go' '*.sh' | xargs grep -L 'SPDX-License-Identifier: Apache-2.0' || true)"; \
	if [ -n "$$missing" ]; then echo "Missing SPDX header:"; echo "$$missing"; exit 1; fi

.PHONY: lint
lint: ## Run golangci-lint (same version as CI).
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: vuln
vuln: ## Report known vulnerabilities reachable from the code (govulncheck).
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

FUZZTIME ?= 30s
.PHONY: fuzz
fuzz: ## Fuzz the config and readiness parsers (FUZZTIME=30s each).
	go test ./internal/otelconfig -run '^$$' -fuzz FuzzAnalyse -fuzztime $(FUZZTIME)
	go test ./internal/readiness -run '^$$' -fuzz FuzzAnalyseRedacts -fuzztime $(FUZZTIME)

.PHONY: verify
verify: fmt-check vet license-check test helm-lint ## Everything CI checks before building an image (plus lint and vuln).

.PHONY: e2e
e2e: ## Smoke test on the current kind cluster: make e2e IMG=collectorctrl-operator:e2e (image loaded with kind load).
	./hack/e2e-kind.sh $(IMG)

.PHONY: vendor-fixtures
vendor-fixtures: ## Re-render vendor Helm chart fixtures (review expectation changes by hand).
	./hack/render-vendor-charts.sh

##@ Build

.PHONY: build
build: ## Build the operator binary into bin/manager.
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/manager ./cmd/operator

.PHONY: run
run: ## Run the operator against the current kubeconfig (needs OPAMP_* env vars).
	go run ./cmd/operator

.PHONY: docker-build
docker-build: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) -f Dockerfile.operator .

.PHONY: docker-push
docker-push: ## Push the container image.
	docker push $(IMG)

.PHONY: docker-buildx
docker-buildx: ## Build and push a multi-arch image (linux/amd64, linux/arm64).
	docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=$(VERSION) -t $(IMG) -f Dockerfile.operator --push .

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart and render it with default and alternative values.
	helm lint $(CHART) $(CHART_TEST_VALUES)
	helm template ci $(CHART) $(CHART_TEST_VALUES) >/dev/null
	helm template ci $(CHART) $(CHART_TEST_VALUES) --set rbac.clusterScoped=false --set clusterID=ci >/dev/null
	helm template ci $(CHART) $(CHART_TEST_VALUES) -f $(CHART)/values-openshift.yaml >/dev/null
	helm template ci $(CHART) $(CHART_TEST_VALUES) --set serviceMonitor.enabled=true | grep -q 'name: ci-collectorctrl-operator-metrics'

.PHONY: helm-template
helm-template: ## Render the chart to stdout.
	helm template collectorctrl-operator $(CHART) $(CHART_TEST_VALUES)

.PHONY: helm-package
helm-package: ## Package the chart into dist/.
	helm package $(CHART) -d dist

.PHONY: clean
clean: ## Remove build output.
	rm -rf bin dist cover.out
