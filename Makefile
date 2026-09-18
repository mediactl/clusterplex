GOBIN ?= $(shell go env GOPATH)/bin
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint-v2
IMG ?= ghcr.io/mediactl/cluster-plex:dev
NETNS_TEST_BIN ?= $(CURDIR)/bin/plexnet.test

.PHONY: all
all: build

##@ Development


.PHONY: proto
proto: ## Regenerate the gRPC bindings from proto/transcoder.proto
	protoc -I proto --go_out=proto --go_opt=paths=source_relative \
		--go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/transcoder.proto

.PHONY: fmt
fmt: ## gofmt the module
	go fmt ./...

.PHONY: vet
vet: ## go vet the module
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint v2
	$(GOLANGCI_LINT) run ./...

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: build
build: ## Build the manager and shim into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/proxy ./cmd/proxy
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance

.PHONY: test
test: ## Unit tests
	go test ./... -coverprofile cover.out

.PHONY: test-netns
test-netns: ## pkg/plexnet against real network namespaces
	# Provisioning a namespace needs CAP_SYS_ADMIN. unshare gives it over the
	# namespaces it creates without needing actual root, and keeps the test's
	# veth pairs and nftables rules off the developer's own network.
	go test -tags netns -c -o $(NETNS_TEST_BIN) ./pkg/plexnet
	unshare --user --map-root-user --net $(NETNS_TEST_BIN) -test.v
	rm -f $(NETNS_TEST_BIN)

##@ Images and clusters

.PHONY: docker-build
docker-build: ## Build the image (the Dockerfile fetches LiteFS itself)
	docker build -f Dockerfile -t $(IMG) .

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint charts/cluster-plex
	helm template cluster-plex charts/cluster-plex >/dev/null

.PHONY: manifests
manifests: ## Render the base manifests
	kubectl kustomize k8s/base

.PHONY: deploy
deploy: ## Apply the base manifests to the current context
	kubectl apply -k k8s/base

.PHONY: deploy-kind
deploy-kind: ## Apply the kind overlay (ReadWriteOnce claims) to the current context
	kubectl apply -k k8s/overlays/kind

.PHONY: kind-up
kind-up: ## Create the kind cluster and namespace
	hack/kind.sh up

.PHONY: kind-load
kind-load: ## Load $(IMG) into the kind cluster
	hack/kind.sh load

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	hack/kind.sh down

.PHONY: e2e
e2e: ## Deploy the kind overlay and run the end-to-end test
	go test -tags e2e ./test/e2e/... -v -timeout 15m

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
