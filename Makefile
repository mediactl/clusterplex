GOBIN ?= $(shell go env GOPATH)/bin
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint-v2
IMG ?= ghcr.io/mediactl/cluster-plex:dev
# release is FROM scratch, with no shell; debug is the same image with busybox
# in /bin, for kubectl exec and the end-to-end suite.
DOCKER_TARGET ?= release
NETNS_TEST_BIN ?= $(CURDIR)/bin/plex-net.test

.PHONY: all
all: build

##@ Development


.PHONY: proto
proto: ## Regenerate the gRPC bindings from proto/transcoder.proto, proto/remux.proto and proto/video.proto
	protoc -I proto --go_out=proto --go_opt=paths=source_relative \
		--go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/transcoder.proto
	mkdir -p proto/remuxpb
	protoc -I proto --go_out=proto/remuxpb --go_opt=paths=source_relative \
		--go-grpc_out=proto/remuxpb --go-grpc_opt=paths=source_relative proto/remux.proto
	mkdir -p proto/videopb
	protoc -I proto --go_out=proto/videopb --go_opt=paths=source_relative \
		--go-grpc_out=proto/videopb --go-grpc_opt=paths=source_relative proto/video.proto

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
build: ## Build the manager, shim, proxy, maintenance trigger, remux worker and video worker into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/proxy ./cmd/proxy
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/remux-worker ./cmd/remux-worker
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/video-worker ./cmd/video-worker

.PHONY: test
test: ## Unit tests
	go test ./... -coverprofile cover.out

# FFGO_DIR is the pinned ffgo's module source, whose shim the native suites
# load, built against the system FFmpeg 9 as images/Dockerfile.remux builds it.
FFGO_DIR = $(shell go list -m -f '{{.Dir}}' github.com/obinnaokechukwu/ffgo)

bin/libffshim.so: go.mod
	mkdir -p bin
	gcc -shared -fPIC -O2 -Wall -DFFSHIM_HAVE_AVDEVICE=1 -DFFSHIM_HAVE_AVFILTER=1 \
		$$(pkg-config --cflags libavutil libavcodec libavformat libavdevice libavfilter) \
		-o $@ $(FFGO_DIR)/shim/ffshim.c \
		$$(pkg-config --libs libavutil libavcodec libavformat libavdevice libavfilter)

.PHONY: test-native
test-native: bin/libffshim.so ## The ffgo suites against the system FFmpeg 9; they fail rather than skip
	FFGO_SHIM_DIR=$(CURDIR)/bin CLUSTERPLEX_REQUIRE_FFGO=1 go test ./pkg/remux/pipeline/... ./pkg/video/...

.PHONY: test-netns
test-netns: ## pkg/plex/net and Live TV's link-local address against real network namespaces
	# Provisioning a namespace needs CAP_SYS_ADMIN. unshare gives it over the
	# namespaces it creates without needing actual root, and keeps the test's
	# veth pairs and nftables rules off the developer's own network.
	go test -tags netns -c -o $(NETNS_TEST_BIN) ./pkg/plex/net
	unshare --user --map-root-user --net $(NETNS_TEST_BIN) -test.v
	rm -f $(NETNS_TEST_BIN)
	go test -tags netns -c -o $(NETNS_TEST_BIN) ./pkg/plex/livetv
	unshare --user --map-root-user --net $(NETNS_TEST_BIN) -test.v -test.run '^TestTheLinkLocalAddress'
	rm -f $(NETNS_TEST_BIN)

##@ Images and clusters

REMUX_IMG ?= ghcr.io/mediactl/cluster-plex-remux:dev
.PHONY: docker-build-remux
docker-build-remux: ## Build the remux worker's image
	docker build -f images/Dockerfile.remux --target remux -t $(REMUX_IMG) .

.PHONY: docker-build
docker-build: ## Build the image; DOCKER_TARGET=debug adds busybox, which make e2e needs
	docker build -f Dockerfile --target $(DOCKER_TARGET) -t $(IMG) .

# A cluster may mix architectures (talos: arm64 control-plane nodes beside
# amd64 workers), so the published images are built for both. The builder
# must run every platform: natively, or arm64 under QEMU (binfmt) on an
# amd64 host. `docker buildx ls` lists what each builder can build, and
# BUILDER picks one other than the current.
PLATFORMS ?= linux/amd64,linux/arm64
BUILDER ?=
BUILDX = docker buildx build $(if $(BUILDER),--builder $(BUILDER)) --platform $(PLATFORMS) --push

.PHONY: docker-buildx
docker-buildx: ## Build and push $(IMG) for $(PLATFORMS)
	$(BUILDX) -f Dockerfile --target $(DOCKER_TARGET) -t $(IMG) .

.PHONY: docker-buildx-remux
docker-buildx-remux: ## Build and push $(REMUX_IMG) for $(PLATFORMS)
	$(BUILDX) -f images/Dockerfile.remux --target remux -t $(REMUX_IMG) .

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint charts/cluster-plex
	# The chart refuses to render without a database host and a password
	# Secret, on purpose; these stand in so the templates themselves are checked.
	helm template cluster-plex charts/cluster-plex \
		--set postgres.host=postgres --set postgres.passwordSecret.name=plex-postgres >/dev/null

# clustarr's chart depends on this one at oci://ghcr.io/mediactl/charts,
# pinned by version: bump charts/cluster-plex/Chart.yaml's version for every
# chart change, then push. A version once pushed is never pushed again.
CHART_REGISTRY ?= oci://ghcr.io/mediactl/charts

.PHONY: chart-push
chart-push: helm-lint ## Package the chart and push it to $(CHART_REGISTRY)
	@mkdir -p bin/charts
	helm package charts/cluster-plex -d bin/charts
	helm push bin/charts/cluster-plex-$$(sed -n 's/^version: //p' charts/cluster-plex/Chart.yaml).tgz $(CHART_REGISTRY)

.PHONY: manifests
manifests: ## Render the base manifests
	kubectl kustomize k8s/base

.PHONY: deploy
deploy: ## Apply the base manifests to the current context
	kubectl apply -k k8s/base

.PHONY: deploy-kind
deploy-kind: ## Apply the kind overlay (ReadWriteOnce claims) to the current context
	kubectl apply -k k8s/overlays/kind

.PHONY: deploy-kind-clustarr
deploy-kind-clustarr: ## Apply the kind overlay plus clustarr stand-ins; refuses a cluster that runs a real clustarr
	hack/deploy-clustarr-fixture.sh

.PHONY: kind-lb
kind-lb: ## Start cloud-provider-kind, so LoadBalancer Services get an address
	hack/kind.sh lb

.PHONY: kind-up
kind-up: ## Create the kind cluster, namespace and load balancer
	hack/kind.sh up

.PHONY: kind-load
kind-load: ## Load $(IMG) into the kind cluster
	hack/kind.sh load

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	hack/kind.sh down

.PHONY: e2e
e2e: ## Deploy the kind overlay and run the end-to-end test
	go test -tags e2e ./test/e2e/... -v -timeout 30m

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
