# Quetzal developer tasks. Mirrors the CI jobs so local == CI.
export GOTOOLCHAIN := local

KIND_CLUSTER ?= quetzal-e2e
# The Kubernetes the e2e suite runs on, here and in CI (which reads it from this
# line): a supported release, not a pin that aged out. 1.31 is long past end of
# life, and an older apiserver also knows fewer things to object to.
KIND_NODE_IMAGE ?= kindest/node:v1.35.0

.PHONY: build test test-postgres lint fmt vet e2e e2e-kind-up e2e-kind-down kind-node-image tidy

build: ## Build all binaries
	go build ./...

test: ## Run unit tests
	go test -race ./...

# The same tests on PostgreSQL, as CI runs them too: each test gets a database
# of its own on the server QUETZAL_TEST_POSTGRES names (see internal/testdb).
#   docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=quetzal postgres:14
#   QUETZAL_TEST_POSTGRES='postgres://postgres:quetzal@localhost:5432/postgres?sslmode=disable' make test-postgres
test-postgres: ## Run unit tests on PostgreSQL
	@test -n "$$QUETZAL_TEST_POSTGRES" || { echo "set QUETZAL_TEST_POSTGRES (see the Makefile)"; exit 1; }
	go test -race ./internal/...

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

kind-node-image: ## Print the kind node image the e2e suite runs on
	@echo $(KIND_NODE_IMAGE)

e2e: ## Run the e2e suite (expects a reachable cluster via KUBECONFIG)
	go test -tags e2e -v -timeout 15m ./test/e2e/...
