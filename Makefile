# mojols: robust Mojo language server.  `make help` lists targets.
GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: help build test race fuzz vet probe zed-check release clean

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build bin/mojols and bin/fakelsp
	cd server && $(GO) build -ldflags '$(LDFLAGS)' -o ../bin/ ./cmd/...

test: ## unit + chaos tests with the race detector
	cd server && $(GO) vet ./... && $(GO) test -race -count=1 ./...

fuzz: ## run each fuzz target for 30s
	cd server && $(GO) test -run='^$$' -fuzz=FuzzReader -fuzztime=30s ./internal/jsonrpc
	cd server && $(GO) test -run='^$$' -fuzz=FuzzOffset -fuzztime=30s ./internal/text

probe: build ## measure the real mojo-lsp-server: make probe FILES="a.mojo b.mojo"
	python3 scripts/m0_probe.py --mojols bin/mojols $(FILES)

zed-check: ## type-check the Zed extension
	cd editors/zed && cargo check

release: ## cross-compile release archives into dist/
	VERSION=$(VERSION) ./scripts/release.sh

clean:
	rm -rf bin dist
