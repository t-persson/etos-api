# Install tools locally instead of in $HOME/go/bin.
export GOBIN := $(CURDIR)/bin
export GOBUILD := go build
BUILD_LDFLAGS ?=
export PATH := $(GOBIN):$(PATH)

export RELEASE_VERSION ?= $(shell git describe --always)
export DOCKER_REGISTRY ?= ghcr.io
export DOCKER_NAMESPACE ?= eiffel-community
export DEPLOY ?= etos-sse

PROGRAMS = sse logarea iut executionspace keys
COMPILEDAEMON = $(GOBIN)/CompileDaemon
GIT = git
GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT = $(GOBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(shell go env GOVERSION)

# Generates a rule for building a single binary from cmd/$1.
#
# $1: Name of the program
define build-binary
.PHONY: $(strip $(1))
$(strip $(1)):
	$$(GOBUILD) -ldflags="-w -s -buildid= $$(BUILD_LDFLAGS)" -trimpath -o bin/$(strip $(1)) ./cmd/$(strip $(1))
endef

# Generates a rule named $(program_name)-docker to to build the named program
# and copy the locally compiled binary into a Docker image. The image will be
# tagged with the name of the program.
#
# $1: Name of the program
define build-docker-image
.PHONY: $(strip $(1))-docker
# Including the parameter name!
EXTRA_DOCKER_ARGS=
export EXTRA_DOCKER_ARGS
$(strip $(1))-docker: $(strip $(1))
	docker build . \
    $$(EXTRA_DOCKER_ARGS) \
		-f deploy/etos-$(strip $(1))/Dockerfile \
		-t $(DOCKER_REGISTRY)/$(DOCKER_NAMESPACE)/etos-$(strip $(1)):$(RELEASE_VERSION)
endef

# Generates a rule named $(program_name)-docker-push to push
# a local Docker image to a remote registry.
#
# $1: Name of the program
define push-docker-image
.PHONY: $(strip $(1))-docker-push
$(strip $(1))-docker-push:
	echo docker push $(DOCKER_REGISTRY)/$(DOCKER_NAMESPACE)/$(strip $(1))_provider:$(RELEASE_VERSION)
endef

.PHONY: all
all: test $(PROGRAMS)

.PHONY: clean
clean:
	$(RM) $(GOBIN)/*
	docker compose --project-directory . -f deploy/$(DEPLOY)/docker-compose.yml rm || true

.PHONY: check
check: staticcheck test

.PHONY: staticcheck
staticcheck: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

.PHONY: test
test:
	go test -cover -timeout 30s -race $(shell go list ./... | grep -v "etos-api/test")

# Start a development docker with a database that restarts on file changes.
.PHONY: start
start: $(COMPILEDAEMON)
	docker compose --project-directory . -f deploy/$(DEPLOY)/docker-compose.yml up

.PHONY: stop
stop:
	docker compose --project-directory . -f deploy/$(DEPLOY)/docker-compose.yml down

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: check-dirty
check-dirty:
	$(GIT) diff --exit-code HEAD

.PHONY: gen
gen:
	go generate ./...


# Setup the dynamic commands
#
$(foreach prog,$(PROGRAMS), \
    $(eval $(call build-binary,$(prog))) \
    $(eval $(call build-docker-image,$(prog))) \
    $(eval $(call push-docker-image,$(prog))) \
)

api-docker:
	docker build . --build-arg BUILD_REVISION=$(shell git rev-parse --short=8 HEAD) -t $(DOCKER_REGISTRY)/$(DOCKER_NAMESPACE)/etos-api:$(RELEASE_VERSION)

sse-docker: EXTRA_DOCKER_ARGS += --build-arg BUILD_REVISION=$(shell git rev-parse --short HEAD)

# Build dependencies

$(COMPILEDAEMON):
	mkdir -p $(dir $@)
	go install github.com/githubnemo/CompileDaemon@v1.3.0

$(GOLANGCI_LINT):
	@mkdir -p "$(GOBIN)"
	@set -eu; \
	tmp=$$(mktemp -d "$(GOBIN)/.golangci-lint-XXXXXX"); \
	trap 'rm -f "$$tmp/golangci-lint"; rmdir "$$tmp"' EXIT; \
	GOBIN="$$tmp" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	mv "$$tmp/golangci-lint" "$@"
