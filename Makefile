.DEFAULT_GOAL := build
SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS_BUILD := -trimpath
TOOL := go tool -modfile=tools.mod
PKGS := ./...
BIN := tailraid

# Release matrix. windows/arm64 is included; drop it here if a dependency fails to build.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build install changelog test test-race test-long test-mount soak soak-full fuzz lint fmt fmt-check vuln \
        release tag check invariants tidy clean help

## build: static binary for the current OS/arch into ./dist
build:
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o dist/$(BIN) ./cmd/tailraid

## install: build and copy the binary to INSTALL_DIR (default ~/.local/bin)
INSTALL_DIR ?= $(HOME)/.local/bin
install: build
	@mkdir -p "$(INSTALL_DIR)" && cp dist/$(BIN) "$(INSTALL_DIR)/$(BIN)" && echo "installed $(INSTALL_DIR)/$(BIN) ($(VERSION))"

## test: fast suite with per-package coverage (what CI runs on every platform, minus -race)
test:
	go test -short -cover $(PKGS)

## test-race: fast suite under the race detector (pre-commit)
test-race:
	go test -short -race $(PKGS)

## test-long: model checker, long durability and oracle runs (minutes; nightly)
test-long:
	go test -race -run 'ModelCheck|Durability|Oracle' -timeout 30m $(PKGS)

# hot-path benchmarks; compare against docs/reports/bench-*.txt with benchstat
bench:
	go test -run '^$$' -bench . -benchmem -count=6 -timeout 30m ./internal/meta ./internal/vfs ./internal/chunk ./internal/ec

## test-mount: real NFS mount workload; needs sudo (run as: sudo -E make test-mount)
test-mount:
	TAILRAID_MOUNT_TEST=1 go test -v -count=1 -run 'TestMount' -timeout 20m ./test/mount/

## soak: four-node docker-compose cluster, scenario driver, report.html + results.jsonl (needs Docker)
soak: soak-binary
	cd test/soak && docker compose up -d --build
	go run ./cmd/soak --profile quick --out report.html --results results.jsonl; \
	status=$$?; cd test/soak && docker compose exec -T n1 tailraid status; docker compose down -v >/dev/null; exit $$status

## soak-binary: static linux binaries for the compose images (arch follows the Docker engine)
soak-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o test/soak/tailraid-linux-arm64 ./cmd/tailraid
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') go build -trimpath -ldflags "-s -w" -o test/soak/soak-client-linux-arm64 ./test/soak/client

## soak-full: the long profile (~1 h)
soak-full: soak-binary
	cd test/soak && docker compose up -d --build
	go run ./cmd/soak --profile full --out report.html --results results.jsonl; \
	status=$$?; cd test/soak && docker compose down -v >/dev/null; exit $$status

## fuzz: run every Fuzz* target for 30 s each
fuzz:
	@set -e; for pkg in $$(go list $(PKGS)); do \
	  for f in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "== $$pkg $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime 30s $$pkg; \
	  done; \
	done

## lint: golangci-lint + invariant/lifecycle citation check
lint:
	$(TOOL) golangci-lint run $(PKGS)
	scripts/check-invariants.sh --check

## invariants: regenerate docs/invariants.md and docs/lifecycle.md
invariants:
	scripts/check-invariants.sh

## fmt: gofmt all Go files in place
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*' -not -path './.git/*')

## fmt-check: fail if any file is not gofmt-clean
fmt-check:
	@out="$$(find . -name '*.go' -not -path './vendor/*' -not -path './.git/*' -exec gofmt -l {} +)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## vuln: govulncheck over all packages
vuln:
	$(TOOL) govulncheck $(PKGS)

## tidy: go mod tidy for both module files
tidy:
	go mod tidy
	go mod tidy -modfile=tools.mod

## changelog: regenerate CHANGELOG.md from the commit log (NEXT=vX.Y.Z names the unreleased section)
changelog:
	scripts/changelog.sh gen $(NEXT)

## tag: cut a release tag after a green check (make tag VERSION=v0.1.0); the changelog commit
## lands first, CI builds the binaries and publishes the release with that section as its notes
tag:
	@case "$(VERSION)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "VERSION must be vMAJOR.MINOR.PATCH"; exit 1;; esac
	@test -z "$$(git status --porcelain)" || { echo "working tree not clean"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "$(VERSION) exists"; exit 1; }
	$(MAKE) check
	scripts/changelog.sh gen $(VERSION)
	@git diff --quiet -- CHANGELOG.md || { git add CHANGELOG.md && git commit -q -m "changelog: $(VERSION)" && git push -q origin HEAD; }
	git tag -a "$(VERSION)" -m "tailraid $(VERSION)"
	git push origin "$(VERSION)"
	@echo "tagged $(VERSION); CI publishes the release at https://github.com/zombar/tailraid/releases/tag/$(VERSION)"

## release: cross-compile static binaries for every platform + sha256sums
release:
	@rm -rf dist && mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
	  out=dist/$(BIN)_$${os}_$${arch}$$ext; echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o $$out ./cmd/tailraid; \
	done
	@cd dist && (command -v sha256sum >/dev/null && sha256sum $(BIN)_* || shasum -a 256 $(BIN)_*) > sha256sums.txt
	@cat dist/sha256sums.txt

## check: everything the pre-commit hook requires
check: fmt-check lint test-race

clean:
	rm -rf dist report.html results.jsonl

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F: '{printf "  %-12s %s\n", $$1, $$2}'
