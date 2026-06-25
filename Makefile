# avairy — build & cross-compile. See BUILD.md for the underlying commands/rationale.
# avairy is pure Go (no CGO), so every target cross-compiles from any host.

CMDS      := avairy avairy-node
DIST      := dist
PLATFORMS := darwin/arm64 darwin/amd64 \
             windows/arm64 windows/amd64 \
             linux/arm64 linux/amd64 \
             freebsd/arm64 freebsd/amd64

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w
# To embed the version, append `-X main.version=$(VERSION)` here and add `var version string`
# to each command's main package.
BUILDFLAGS := -trimpath -ldflags '$(LDFLAGS)'
export CGO_ENABLED := 0

.PHONY: all build test vet fmt tidy check release clean help

all: build

## build: native binaries for the host into dist/<os>-<arch>/
build:
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
	ext=; if [ "$$os" = windows ]; then ext=.exe; fi; \
	dir=$(DIST)/$$os-$$arch; mkdir -p $$dir; \
	for c in $(CMDS); do \
		echo "building $$dir/$$c$$ext"; \
		go build $(BUILDFLAGS) -o $$dir/$$c$$ext ./cmd/$$c || exit 1; \
	done

## test: run the test suite
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format sources
fmt:
	gofmt -w ./internal ./cmd

## tidy: tidy modules
tidy:
	go mod tidy

## check: fmt + vet + test
check: fmt vet test

## release: cross-compile every command for every target into dist/<os>-<arch>/
release:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=; if [ "$$os" = windows ]; then ext=.exe; fi; \
		dir=$(DIST)/$$os-$$arch; mkdir -p $$dir; \
		for c in $(CMDS); do \
			out=$$dir/$$c$$ext; \
			echo "building $$out"; \
			GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -o $$out ./cmd/$$c || exit 1; \
		done; \
	done
	@echo "done → $(DIST)/ (version $(VERSION))"

## clean: remove build artifacts
clean:
	rm -rf $(DIST)

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
