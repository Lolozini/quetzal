# Quetzal developer tasks. Mirrors the CI jobs so local == CI.
export GOTOOLCHAIN := local

KIND_CLUSTER ?= quetzal-e2e
KIND_NODE_IMAGE ?= kindest/node:v1.31.0

.PHONY: build test lint fmt vet e2e e2e-kind-up e2e-kind-down tidy

build: ## Build all binaries
	go build ./...

test: ## Run unit tests
	go test -race ./...

lint: fmt-check vet ## gofmt check + go vet

fmt: ## Format the code
	gofmt -w .

fmt-check: ## Fail if code is not gofmt-ed
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "Not gofmt-ed:"; echo "$$unformatted"; exit 1; fi

vet:
	go vet ./...

tidy:
	go mod tidy

## --- end-to-end against a local kind cluster ---

e2e-kind-up: ## Create a disposable kind cluster
	kind create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE)

e2e-kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

e2e: ## Run the e2e suite (expects a reachable cluster via KUBECONFIG)
	go test -tags e2e -v -timeout 15m ./test/e2e/...
