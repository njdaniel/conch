GO      ?= go
LINT    ?= golangci-lint

.PHONY: build test lint vet fmt fmt-check check schema-compat depgate hooks-install clean

build:
	$(GO) build ./...
	$(GO) build -o bin/conchd ./cmd/conchd
	$(GO) build -o bin/conch ./cmd/conch

test:
	$(GO) test ./...

lint:
	$(LINT) run

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

check: fmt-check vet lint test schema-compat depgate

schema-compat:
	./scripts/schema-compat.sh

depgate:
	./scripts/depgate.sh

hooks-install:
	git config core.hooksPath .githooks
	@echo "git hooks installed (.githooks)"

clean:
	rm -rf bin/
