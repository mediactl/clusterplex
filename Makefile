GOBIN ?= $(shell go env GOPATH)/bin
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint-v2
IMG ?= ghcr.io/mediactl/cluster-plex:dev

.PHONY: all
all: build

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim

.PHONY: docker-build
docker-build:
	docker build -f Dockerfile -t $(IMG) .

.PHONY: test
test:
	go test ./... -coverprofile cover.out

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)


.PHONY: kind-up
kind-up:
	hack/kind.sh up

.PHONY: kind-down
kind-down:
	hack/kind.sh down

.PHONY: e2e
e2e:
	go test ./test/e2e/... -v -timeout 10m
