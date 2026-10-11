GO      ?= go
# Override GNU Make's built-in LINT=lint; command-line overrides still work.
# Whatever LINT names must be the version in .golangci-lint-version, the one
# CI installs; scripts/lint-version.sh fails the lint target otherwise.
LINT    = golangci-lint

.PHONY: build test lint vet fmt fmt-check check schema-compat depgate rust-check rust-build hooks-install clean

build:
	$(GO) build ./...
	$(GO) build -o bin/conchd ./cmd/conchd
	$(GO) build -o bin/conch ./cmd/conch
	$(GO) build -o bin/conch-bot ./cmd/conch-bot

test:
	$(GO) test ./...

lint:
	@./scripts/lint-version.sh $(LINT)
	$(LINT) run

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

check: fmt-check vet lint test schema-compat depgate rust-check

# The Rust voice client under voice/ (ADR-006): fmt, clippy, tests, the
# dependency gate and cargo-deny, with the pinned toolchain. Fetch that once
# with scripts/voice-toolchain.sh; without it this fails, it does not skip.
rust-check:
	./scripts/rust-check.sh

# Builds bin/conch-voice. Not part of `build`: conchd and conch need no Rust.
rust-build:
	@mkdir -p bin
	bash -c '. ./scripts/voice-env.sh && cd voice && cargo build --locked --release && cp target/release/conch-voice ../bin/conch-voice'

schema-compat:
	./scripts/schema-compat.sh

depgate:
	./scripts/depgate.sh

hooks-install:
	git config core.hooksPath .githooks
	@echo "git hooks installed (.githooks)"

clean:
	rm -rf bin/ voice/target/
