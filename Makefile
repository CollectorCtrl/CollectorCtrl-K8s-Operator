# CollectorCtrl Kubernetes Operator
#
# Every target here works from a clean checkout with Go, Docker and Helm.

IMG ?= ghcr.io/collectorctrl/collectorctrl-k8s-operator/operator:dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART := helm-charts/collectorctrl-operator
# Values needed to render the chart (the chart refuses to render without a credential source).
CHART_TEST_VALUES := --set opamp.enrollmentSecret=collectorctrl-auth

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

.PHONY: verify
verify: fmt-check vet test helm-lint ## Everything CI checks before building an image.

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
