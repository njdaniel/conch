GO      ?= go
# Override GNU Make's built-in LINT=lint; command-line overrides still work.
# Whatever LINT names must be the version in .golangci-lint-version, the one
# CI installs; scripts/lint-version.sh fails the lint target otherwise.
LINT    = golangci-lint

# Local builds say which commit they are (issue #425); release.yml stamps the
# tag instead. Outside a git checkout this is empty and the binary falls back
# to its embedded build info. Use this explicit stamp in linked worktrees:
# unstamped Go builds can read the containing main checkout's VCS metadata.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS  = -X main.version=$(VERSION)

.PHONY: build test lint vet fmt fmt-check invisible-check check schema-compat schema-ts depgate deploy-test rust-check rust-build web-node web-check web-build web-embed build-web-embedded web-e2e desktop-node desktop-check desktop-build desktop-smoke p3-local p3-reset hooks-install clean

build:
	$(GO) build ./...
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/conchd ./cmd/conchd
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/conch ./cmd/conch
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

check: fmt-check invisible-check vet lint test schema-compat schema-ts depgate deploy-test rust-check web-check desktop-check

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
web-node web-check web-build web-embed web-e2e: export PATH := $(if $(wildcard $(WEB_NODE_BIN)/node),$(WEB_NODE_BIN):)$(PATH)

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

# The browser suite (issue #285, Playwright, Chromium only): the web client in a
# real browser against the fixture community of e2e/p3-local. It is not part of
# `make check`, which needs no browser. The browser is the one Playwright's own
# version pins; fetch it once with `cd web && npx playwright install chromium`
# (CI uses --with-deps, which needs root; do not here). The suite starts and
# removes its own server and root, and leaves nothing running.
web-e2e: web-node
	cd web && npm ci --no-audit
	@cd web && node --input-type=module -e "import { chromium } from '@playwright/test'; import { existsSync } from 'node:fs'; if (!existsSync(chromium.executablePath())) { console.error('web-e2e: FAIL: the browser Playwright pins is not installed; run: cd web && npx playwright install chromium'); process.exit(1); }"
	cd web && npm run e2e

# The desktop shell under desktop/ (issue #940, ADR-007, linux-desktop.md): an
# Electron app that serves the web client from packaged files. desktop-check
# runs the repository's gates for it (the fetch script's test, the lint rules,
# the dependency and licence gates), then npm ci, typecheck, lint and unit
# tests. It downloads no Electron binary and needs no display. Only `npm ci`
# installs, from the committed lockfile. It needs the Node desktop/.nvmrc pins;
# without it this fails and names that version, it does not skip.
DESKTOP_NODE = $(shell cat desktop/.nvmrc)
DESKTOP_NODE_BIN = $(HOME)/.nvm/versions/node/v$(DESKTOP_NODE)/bin
desktop-node desktop-check desktop-build desktop-smoke: export PATH := $(if $(wildcard $(DESKTOP_NODE_BIN)/node),$(DESKTOP_NODE_BIN):)$(PATH)

desktop-node:
	@v=$$(node --version 2>/dev/null) || { echo "desktop: FAIL: Node.js is not installed; install Node $(DESKTOP_NODE) (desktop/.nvmrc)"; exit 1; }; \
	if [ "$$v" != "v$(DESKTOP_NODE)" ]; then echo "desktop: FAIL: Node $$v is installed; install Node $(DESKTOP_NODE) (desktop/.nvmrc)"; exit 1; fi

desktop-check: desktop-node
	./scripts/desktop-electron-test.sh
	./scripts/desktop-lint-test.sh
	./scripts/desktop-lint.sh
	./scripts/web-depgate.sh desktop
	./scripts/web-licences.sh desktop/package-lock.json desktop/licences-allowlist.txt
	cd desktop && npm ci --no-audit
	cd desktop && npm run typecheck
	cd desktop && npm run lint
	cd desktop && npm test

# The unpacked Linux app, desktop/dist/linux-unpacked/. Fetches the pinned
# Electron into ~/.cache/conch once (scripts/desktop-electron.sh checks its
# SHA-256 twice) and builds web/ for it without touching web/dist. Not part of
# `make check`.
desktop-build: desktop-node
	./scripts/desktop-build.sh

# The packaged smoke test: drives the built app over Chromium's remote-debugging
# port (desktop/tests/smoke/launch.ts says why). Needs a display: CI runs it
# under xvfb-run; CONCH_SMOKE_HEADLESS=1 uses Chromium's headless Ozone
# platform where there is no X server. Not part of `make check`.
desktop-smoke: desktop-node
	cd desktop && npm ci --no-audit
	cd desktop && npm run smoke

# The P3 local prototype (issue #283, docs/p3-walkthrough.md): p3-local builds
# the binaries and the web client, starts a disposable conchd, creates the test
# community and prints one command per participant, leaving it running;
# p3-reset stops it and removes everything it made. Neither touches
# ~/.config/conch.
p3-local:
	$(GO) run ./e2e/p3-local --keep

p3-reset:
	$(GO) run ./e2e/p3-local --reset

# Builds web/dist, which `conchd serve --web-dir web/dist` serves at /app/.
# Not part of `build`: conchd and conch need no Node.
web-build: web-node
	cd web && npm ci --no-audit && npm run build

# Copies the build into internal/web/dist, which a conchd built with
# -tags conchweb embeds. internal/web/dist is git-ignored. Not part of `build`.
web-embed: web-build
	rm -rf internal/web/dist
	mkdir -p internal/web
	cp -r web/dist internal/web/dist

# bin/conchd with the web client inside it, served at /app/ with no --web-dir.
# It needs Node (web-build); `make build` does not. Without the tag a binary
# has no client, and with the tag but no internal/web/dist the compile fails,
# which is intended.
build-web-embedded: web-embed
	$(GO) build -tags conchweb -ldflags "$(LDFLAGS)" -o bin/conchd ./cmd/conchd

schema-compat:
	./scripts/schema-compat.sh

# The web client's wire types are generated from pkg/schema by
# `go generate ./pkg/schema` (issue #348); this fails when the committed
# web/src/schema/generated*.ts differ from what that writes. Go only, no Node.
# The script's own test runs first.
schema-ts:
	./scripts/schema-ts-check-test.sh
	./scripts/schema-ts-check.sh

depgate:
	./scripts/depgate.sh

# The deployment files in ops/deploy (issue #435): shellcheck, systemd-analyze
# verify, and the backup, restore, upgrade and rollback scripts run against a
# real conchd. Needs no droplet. sqlite3 is a host prerequisite of the scripts.
deploy-test:
	./scripts/deploy-scripts-test.sh

hooks-install:
	git config core.hooksPath .githooks
	@echo "git hooks installed (.githooks)"

clean:
	rm -rf bin/ voice/target/
