# avairy — build & cross-compile. See BUILD.md for the underlying commands/rationale.
# avairy is pure Go (no CGO), so every target cross-compiles from any host.

CMDS      := avairy
DIST      := dist
PLATFORMS := darwin/arm64 darwin/amd64 \
             windows/arm64 windows/amd64 \
             linux/arm64 linux/amd64 \
             freebsd/arm64 freebsd/amd64

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Version metadata is stamped into internal/buildinfo at link time (shown by `avairy version`).
# CI passes VERSION/COMMIT/DATE in; a local build derives them from git.
BI        := avairy/internal/buildinfo
LDFLAGS   := -s -w -X $(BI).Version=$(VERSION) -X $(BI).Commit=$(COMMIT) -X $(BI).Date=$(DATE)
BUILDFLAGS := -trimpath -ldflags '$(LDFLAGS)'
export CGO_ENABLED := 0

.PHONY: all build test vet fmt tidy check release package clean help

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

## fmt: format Go sources (gofmt) and the web console HTML/CSS/JS (prettier)
fmt:
	gofmt -w ./internal ./cmd
	@if command -v npx >/dev/null 2>&1; then \
		npx --yes prettier@3.9.1 --write internal/operator/web/ ; \
	else \
		echo "fmt: prettier skipped — install Node (npx) to format the web console"; \
	fi

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

## package: release-build, then archive each target (.tar.gz / .zip) + SHA256SUMS into dist/
package: release
	@cd $(DIST); rm -f *.tar.gz *.zip SHA256SUMS; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir=$$os-$$arch; \
		base=avairy_$(VERSION)_$${os}_$${arch}; \
		if [ "$$os" = windows ]; then \
			( cd $$dir && zip -q ../$$base.zip $(addsuffix .exe,$(CMDS)) ); \
			echo "packaged $$base.zip"; \
		else \
			tar -czf $$base.tar.gz -C $$dir $(CMDS); \
			echo "packaged $$base.tar.gz"; \
		fi; \
	done; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum *.tar.gz *.zip > SHA256SUMS; \
	else shasum -a 256 *.tar.gz *.zip > SHA256SUMS; fi; \
	echo "checksums → $(DIST)/SHA256SUMS"

## clean: remove build artifacts
clean:
	rm -rf $(DIST)

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
