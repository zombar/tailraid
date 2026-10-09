.DEFAULT_GOAL := build
SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS_BUILD := -trimpath -mod=vendor
# Dev tools live in tools.mod. With vendor/ present, `go tool -modfile` refuses to run
# ("inconsistent vendoring": the vendor tree matches go.mod, not tools.mod), so tools run
# through `go run -mod=readonly`, which ignores vendor/ and never edits a module file.
TOOL := go run -mod=readonly -modfile=tools.mod
GOLANGCI := $(TOOL) github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOVULNCHECK := $(TOOL) golang.org/x/vuln/cmd/govulncheck
DEADCODE := $(TOOL) golang.org/x/tools/cmd/deadcode
UNPARAM := $(TOOL) mvdan.cc/unparam
TSGO := $(TOOL) github.com/microsoft/typescript-go/cmd/tsgo
MISSPELL := $(TOOL) github.com/golangci/misspell/cmd/misspell
# actionlint has its own modfile: it needs go.yaml.in/yaml/v4 rc.3 and gosec (in
# golangci-lint) needs rc.6, and the two release candidates differ in API.
ACTIONLINT := go run -mod=readonly -modfile=actionlint.mod github.com/rhysd/actionlint/cmd/actionlint
YAMLFMT := $(TOOL) github.com/google/yamlfmt/cmd/yamlfmt
LEFTHOOK := $(TOOL) github.com/evilmartians/lefthook/v2
PKGS := ./...
BIN := tailraid
# Prose and markup the spell check and the docs script cover.
DOCS := README.md CLAUDE.md CHANGELOG.md docs/*.md docs/windows-checklist.md
WEB_TEXT := internal/webui/src/*.ts internal/webui/src/app.css internal/webui/static/index.html docs/assets/lookdev/*.html
YAML := .github/workflows/*.yml .golangci.yml lefthook.yml .yamlfmt test/soak/docker-compose*.yml
SHELL_SCRIPTS := scripts/*.sh test/soak/entry.sh

# Release matrix. windows/arm64 is included; drop it here if a dependency fails to build.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build install changelog test test-race test-long test-mount soak soak-single soak-full fuzz lint fmt fmt-check vuln \
        release tag check invariants tidy vendor-check deadcode lint-cross clean help web web-check web-typecheck \
        spell docs-check actionlint yaml-check yamlfmt shell-check hooks

# Browser UI (docs/13 §6): esbuild is a Go tool; the bundle is committed and embedded.
WEB_SRC := internal/webui/src
WEB_DIST := internal/webui/dist
ESBUILD := $(TOOL) github.com/evanw/esbuild/cmd/esbuild $(WEB_SRC)/main.ts --bundle --format=esm --target=es2022 --platform=browser \
           --log-level=warning --legal-comments=none

## build: static binary for the current OS/arch into ./dist
build:
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o dist/$(BIN) ./cmd/tailraid

## install: build and copy the binary to INSTALL_DIR (default ~/.local/bin)
INSTALL_DIR ?= $(HOME)/.local/bin
# copy beside, then rename: overwriting the file in place keeps its inode, and macOS
# kills the new binary on launch because the kernel cached the old code signature for it
install: build
	@mkdir -p "$(INSTALL_DIR)" && cp dist/$(BIN) "$(INSTALL_DIR)/.$(BIN).new" && mv -f "$(INSTALL_DIR)/.$(BIN).new" "$(INSTALL_DIR)/$(BIN)" && echo "installed $(INSTALL_DIR)/$(BIN) ($(VERSION))"

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

## soak: four-node docker-compose cluster (two added disks each), scenario driver, report.html + results.jsonl (needs Docker)
soak: soak-binary
	cd test/soak && docker compose up -d --build
	go run ./cmd/soak --profile quick --out report.html --results results.jsonl; \
	status=$$?; cd test/soak && docker compose exec -T n1 tailraid status; docker compose down -v >/dev/null; exit $$status

## soak-binary: static linux binaries for the compose images (arch follows the Docker engine)
soak-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o test/soak/tailraid-linux-arm64 ./cmd/tailraid
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') go build -trimpath -ldflags "-s -w" -o test/soak/soak-client-linux-arm64 ./test/soak/client

## soak-single: one node with four added disks (docs/14), the single-machine volume
soak-single: soak-binary
	cd test/soak && docker compose -f docker-compose.single.yml up -d --build
	go run ./cmd/soak --profile quick --topology single --out report-single.html --results results-single.jsonl; \
	status=$$?; cd test/soak && docker compose -f docker-compose.single.yml exec -T n1 tailraid status; docker compose -f docker-compose.single.yml down -v >/dev/null; exit $$status

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

## web: bundle the browser UI (internal/webui/src → internal/webui/dist/app.js, committed)
web:
	$(ESBUILD) --outfile=$(WEB_DIST)/app.js

## web-check: the committed bundle matches the sources (part of check)
web-check:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(ESBUILD) --outfile=$$tmp/app.js && cmp -s $$tmp/app.js $(WEB_DIST)/app.js \
	  || { echo "$(WEB_DIST)/app.js is stale; run make web"; exit 1; }

## web-typecheck: the TypeScript type-checks (esbuild only bundles; typescript-go checks)
web-typecheck:
	$(TSGO) -p internal/webui/tsconfig.json --noEmit

## lint: golangci-lint + invariant/lifecycle citations + dead code + docs, YAML, workflow and shell checks
lint:
	$(GOLANGCI) run $(PKGS)
	scripts/check-invariants.sh --check
	$(MAKE) --no-print-directory deadcode docs-check spell actionlint yaml-check shell-check

## spell: misspell over docs, README, TypeScript, CSS and the lookdev HTML
spell:
	$(MISSPELL) -error $(DOCS) $(WEB_TEXT)

## docs-check: links, anchors, invariant mentions, fences and headings in the docs (scripts/check-docs.sh)
docs-check:
	scripts/check-docs.sh

## actionlint: the GitHub workflow files
actionlint:
	$(ACTIONLINT) .github/workflows/*.yml

## yaml-check: YAML files are yamlfmt-clean (make yamlfmt rewrites them)
yaml-check:
	$(YAMLFMT) -lint $(YAML)

yamlfmt:
	$(YAMLFMT) $(YAML)

## shell-check: shell scripts parse (bash -n; shellcheck is optional and not required)
shell-check:
	@for f in $(SHELL_SCRIPTS); do bash -n "$$f" || exit 1; done

## hooks: install the lefthook git hooks for this clone (pre-commit, pre-push, commit-msg)
hooks:
	$(LEFTHOOK) install

## lint-cross: golangci-lint under GOOS=darwin and GOOS=windows, so the per-OS files are
## linted too. The linter itself is built for the host first: `GOOS=darwin go run` would
## build a darwin linter that cannot run here.
GOLANGCI_BIN := dist/tools/golangci-lint
$(GOLANGCI_BIN): tools.mod tools.sum
	GOOS=$$(go env GOHOSTOS) GOARCH=$$(go env GOHOSTARCH) go build -mod=readonly -modfile=tools.mod -o $@ github.com/golangci/golangci-lint/v2/cmd/golangci-lint
lint-cross: $(GOLANGCI_BIN)
	GOOS=darwin $(GOLANGCI_BIN) run $(PKGS)
	GOOS=windows $(GOLANGCI_BIN) run $(PKGS)

## deadcode: nothing unreachable from the binaries and tests, no unused parameters
deadcode:
	@out="$$($(DEADCODE) -test $(PKGS))"; if [ -n "$$out" ]; then echo "$$out"; echo "deadcode: unreachable from the binaries and tests"; exit 1; fi
	@out="$$($(UNPARAM) $(PKGS) 2>&1 | grep -v '^exit status')"; if [ -n "$$out" ]; then echo "$$out"; echo "unparam: unused parameters or results"; exit 1; fi

## invariants: regenerate docs/invariants.md and docs/lifecycle.md
invariants:
	scripts/check-invariants.sh

## fmt: gofmt all Go files in place (vendor/ is upstream code, left as it is)
fmt:
	gofmt -w $$(git ls-files -co --exclude-standard -- '*.go' ':!vendor')

## fmt-check: fail if any file is not gofmt-clean
fmt-check:
	@out="$$(git ls-files -co --exclude-standard -- '*.go' ':!vendor' | xargs gofmt -l)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## vuln: govulncheck over all packages
vuln:
	$(GOVULNCHECK) $(PKGS)

## tidy: go mod tidy for every module file, then refresh vendor/
tidy:
	go mod tidy
	go mod tidy -modfile=tools.mod
	go mod tidy -modfile=actionlint.mod
	go mod vendor

## vendor-check: vendor/ matches go.mod (part of check and CI)
vendor-check:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	go mod vendor -o "$$tmp/vendor" && diff -qr "$$tmp/vendor" vendor >/dev/null \
	  || { echo "vendor/ is stale; run make tidy"; diff -qr "$$tmp/vendor" vendor | head; exit 1; }

## changelog: regenerate CHANGELOG.md from the commit log (NEXT=vX.Y.Z names the unreleased section)
changelog:
	scripts/changelog.sh gen $(NEXT)

## tag: cut a release tag after a green check (make tag VERSION=v0.1.0); the changelog commit
## lands first, CI builds the binaries and publishes the release with that section as its notes
tag:
	@case "$(VERSION)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "VERSION must be vMAJOR.MINOR.PATCH"; exit 1;; esac
	@test "$$(git rev-parse --abbrev-ref HEAD)" = main || { echo "run from main"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree not clean"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "$(VERSION) exists"; exit 1; }
	$(MAKE) check
	scripts/changelog.sh gen $(VERSION)
	@# main is protected: a changed changelog goes up as a pull request first. The branch is
	@# named after the commit, so a rerun after more merges never collides with an earlier
	@# one, and any failing step stops here with git's own error, back on a clean main.
	@if ! git diff --quiet -- CHANGELOG.md; then \
	  branch=changelog/$(VERSION)-$$(git rev-parse --short HEAD); \
	  if git checkout -q -b "$$branch" && git add CHANGELOG.md && git commit -q -m "changelog: $(VERSION)" && \
	     git push -q -u origin "$$branch" && \
	     gh pr create --base main --head "$$branch" --title "changelog: $(VERSION)" \
	       --body "CHANGELOG.md for $(VERSION), generated by make tag. Merge it, then on an updated main run: make tag VERSION=$(VERSION)"; then \
	    git checkout -q main; \
	    echo "main is protected: the changelog went up as a pull request ($$branch). Merge it, git pull, then run make tag VERSION=$(VERSION) again to cut the tag."; \
	    exit 2; \
	  fi; \
	  echo "make tag: opening the changelog pull request failed (see the error above); nothing was tagged." >&2; \
	  git checkout -q main 2>/dev/null; git checkout -q -- CHANGELOG.md; git branch -q -D "$$branch" 2>/dev/null; \
	  exit 1; \
	fi
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
check: fmt-check vendor-check lint web-check web-typecheck test-race

clean:
	rm -rf dist report.html results.jsonl

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F: '{printf "  %-12s %s\n", $$1, $$2}'
