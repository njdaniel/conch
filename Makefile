GO      ?= go
# Override GNU Make's built-in LINT=lint; command-line overrides still work.
# Whatever LINT names must be the version in .golangci-lint-version, the one
# CI installs; scripts/lint-version.sh fails the lint target otherwise.
LINT    = golangci-lint

.PHONY: build test lint vet fmt fmt-check invisible-check check schema-compat depgate rust-check rust-build web-node web-check web-build hooks-install clean

build:
	$(GO) build ./...
	$(GO) build -o bin/conchd ./cmd/conchd
	$(GO) build -o bin/conch ./cmd/conch
	$(GO) build -o bin/conch-adapter ./cmd/conch-adapter
	$(GO) build -o bin/conch-bot ./cmd/conch-bot

test:
	$(GO) test ./...

lint:
	@./scripts/lint-version.sh $(LINT)
	$(LINT) run

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(git ls-files -co --exclude-standard '*.go')

fmt-check:
	@out=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

check: fmt-check invisible-check vet lint test schema-compat depgate rust-check web-check

# No tracked text file may hold a bidi control, a line or paragraph separator
# or a control character other than tab, LF and CR (issue #244). The script's
# own test runs first, so a check that has stopped checking fails here too.
invisible-check:
	./scripts/invisible-chars-test.sh
	./scripts/invisible-chars.sh

# The Rust voice client under voice/ (ADR-006): fmt, clippy, tests, the
# dependency gate and cargo-deny, with the pinned toolchain. Fetch that once
# with scripts/voice-toolchain.sh; without it this fails, it does not skip.
rust-check:
	./scripts/rust-check.sh

# Builds bin/conch-voice. Not part of `build`: conchd and conch need no Rust.
rust-build:
	@mkdir -p bin
	bash -c '. ./scripts/voice-env.sh && cd voice && cargo build --locked --release && cp target/release/conch-voice ../bin/conch-voice'

# The web client under web/ (ADR-007): the gates' own tests, the dependency
# and licence gates (before anything is downloaded), then typecheck, lint and
# unit tests. Only `npm ci` installs, from the committed lockfile. It needs the
# Node web/.nvmrc pins; without it this fails and names that version, it does
# not skip. It downloads no browser.
WEB_NODE = $(shell cat web/.nvmrc)
# When nvm has the pinned Node, the web targets use it, whatever node a shell
# profile puts first on PATH; otherwise the node on PATH, checked below.
WEB_NODE_BIN = $(HOME)/.nvm/versions/node/v$(WEB_NODE)/bin
web-node web-check web-build: export PATH := $(if $(wildcard $(WEB_NODE_BIN)/node),$(WEB_NODE_BIN):)$(PATH)

web-node:
	@v=$$(node --version 2>/dev/null) || { echo "web: FAIL: Node.js is not installed; install Node $(WEB_NODE) (web/.nvmrc)"; exit 1; }; \
	if [ "$$v" != "v$(WEB_NODE)" ]; then echo "web: FAIL: Node $$v is installed; install Node $(WEB_NODE) (web/.nvmrc)"; exit 1; fi

web-check: web-node
	./scripts/web-gates-test.sh
	./scripts/web-depgate.sh
	./scripts/web-licences.sh
	cd web && npm ci --no-audit
	cd web && npm run typecheck
	cd web && npm run lint
	cd web && npm test

# Builds web/dist, which `conchd serve --web-dir web/dist` serves at /app/.
# Not part of `build`: conchd and conch need no Node.
web-build: web-node
	cd web && npm ci --no-audit && npm run build

schema-compat:
	./scripts/schema-compat.sh

depgate:
	./scripts/depgate.sh

hooks-install:
	git config core.hooksPath .githooks
	@echo "git hooks installed (.githooks)"

clean:
	rm -rf bin/ voice/target/
