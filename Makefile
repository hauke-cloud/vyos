# Makefile for the VyOS container image

IMAGE      ?= ghcr.io/hauke-cloud/vyos
IMAGE_TAG  ?= dev

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Container engine. podman and docker are interchangeable here; docker only
# counts if its daemon answers, a docker CLI without one is common.
CONTAINER_ENGINE ?= $(shell docker info >/dev/null 2>&1 && echo docker || command -v podman 2>/dev/null)

# Pinned so that a local run and a CI run report the same findings.
GOLANGCI_LINT_VERSION ?= v2.14.0
GOLANGCI_LINT         ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# The vyos-build commit scripts/iso-to-oci was copied from.
ISO_TO_OCI_COMMIT ?= f5a1e8334f772225d9e3ccdab45f6884cec28e59

ROOTFS := build/rootfs.tar.xz

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Failover helper

.PHONY: build
build: ## Build hcloud-vrrp-failover for the host
	CGO_ENABLED=0 go build -trimpath -ldflags="-w -s -X main.version=$(VERSION)" \
	  -o bin/hcloud-vrrp-failover ./cmd/hcloud-vrrp-failover

.PHONY: test
test: ## Run the Go tests with the race detector
	go test -race -count=1 ./...

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
	@# `gofmt -l` only lists offenders and still exits 0, so the check has to
	@# be inverted to be able to fail.
	@unformatted="$$(gofmt -s -l .)"; \
	if [ -n "$$unformatted" ]; then \
	  echo "not gofmt-clean:"; echo "$$unformatted"; gofmt -s -d .; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: ci-lint
ci-lint: fmt-check vet lint ## Every lint CI runs, in one target

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: fmt ci-lint test ## Everything CI runs on the Go code

##@ Image

# The ISO is half a gigabyte and the conversion takes a minute, so the tarball
# is only rebuilt when the pin or the scripts change.
$(ROOTFS): VYOS_VERSION scripts/build-rootfs.sh scripts/iso-to-oci keys/vyos-nightly.pub
	scripts/build-rootfs.sh

.PHONY: rootfs
rootfs: $(ROOTFS) ## Download and verify the pinned ISO, convert it to a rootfs tarball

# --format docker: podman drops HEALTHCHECK from an OCI-format image.
IMAGE_FORMAT := $(if $(findstring podman,$(CONTAINER_ENGINE)),--format docker,)

.PHONY: container-engine
container-engine:
	@test -n "$(CONTAINER_ENGINE)" || { \
	  echo "no container engine found: start docker, install podman, or set CONTAINER_ENGINE" >&2; exit 1; }

.PHONY: image
image: container-engine $(ROOTFS) ## Build the container image for the host platform
	$(CONTAINER_ENGINE) build $(IMAGE_FORMAT) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  -t $(IMAGE):$(IMAGE_TAG) .

.PHONY: smoke
smoke: container-engine ## Boot the image and round-trip a configuration through the REST API
	CONTAINER_ENGINE=$(CONTAINER_ENGINE) test/smoke.sh $(IMAGE):$(IMAGE_TAG)

.PHONY: ci-image
ci-image: rootfs image smoke ## What CI runs before it builds and pushes the image

.PHONY: update-iso-to-oci
update-iso-to-oci: ## Re-copy scripts/iso-to-oci from vyos-build at ISO_TO_OCI_COMMIT
	curl --fail --silent --show-error --location --output scripts/iso-to-oci \
	  https://raw.githubusercontent.com/vyos/vyos-build/$(ISO_TO_OCI_COMMIT)/scripts/iso-to-oci
	chmod +x scripts/iso-to-oci

.PHONY: clean
clean: ## Remove build artefacts, keep the downloaded ISO
	rm -rf bin build coverage.out

.PHONY: distclean
distclean: clean ## Remove build artefacts and the ISO cache
	rm -rf .cache
