# Makefile for the multichain-rpc-gateway
# (Go module github.com/xtianxx/multichain-rpc-gateway)
#
# Targets: demo, demo-failover, test, test-conformance, lint, bench, load
# Expected behavior per target:
#   specs/001-multichain-rpc-routing/quickstart.md

GO       ?= go
BIN_DIR  ?= bin
GATEWAY  := $(BIN_DIR)/gateway
MOCKUP   := $(BIN_DIR)/mockupstream

# vegeta load-test settings (quickstart.md §4: 1000 req/s, 60s, both rounds)
LOAD_RATE         ?= 1000
LOAD_DURATION     ?= 60s
LOAD_GATEWAY_URL  ?= http://localhost:8545
LOAD_DIRECT_URL   ?= http://localhost:19545  # mock upstream port used by scripts/demo.sh

.PHONY: demo demo-failover test test-conformance lint bench load clean

## demo: start 2 mock upstreams (chain 1 / 8453) + gateway, print test commands
demo: $(GATEWAY) $(MOCKUP)
	./scripts/demo.sh

## demo-failover: end-to-end failover demo (mock upstream fault injection)
demo-failover: $(GATEWAY) $(MOCKUP)
	./scripts/demo-failover.sh

## test: unit (routing/validation/breaker/backoff) + integration (in-process mock, both chains)
test:
	$(GO) test ./...

## test-conformance: JSON-RPC 2.0 conformance vector suite
test-conformance:
	$(GO) test -run TestConformance ./tests/conformance/

## lint: go vet + gofmt check (same gates as CI)
lint:
	$(GO) vet ./...
	gofmt -l .

## bench: in-process passthrough overhead benchmark (direct vs gateway)
bench:
	$(GO) test -bench . -benchtime=10s -count=5 ./bench/

## load: sustained load via vegeta — gateway round + direct baseline round
load: $(GATEWAY)
	@echo "== load: via gateway ($(LOAD_GATEWAY_URL)) =="
	@printf 'POST %s\nContent-Type: application/json\nX-Chain-Id: 1\n\n{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}\n' '$(LOAD_GATEWAY_URL)' | \
		vegeta attack -rate=$(LOAD_RATE) -duration=$(LOAD_DURATION) -output=/tmp/gateway-load.bin
	@vegeta report -type=text /tmp/gateway-load.bin
	@echo "== load: direct to upstream ($(LOAD_DIRECT_URL)) =="
	@printf 'POST %s\nContent-Type: application/json\n\n{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}\n' '$(LOAD_DIRECT_URL)' | \
		vegeta attack -rate=$(LOAD_RATE) -duration=$(LOAD_DURATION) -output=/tmp/direct-load.bin
	@vegeta report -type=text /tmp/direct-load.bin

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR)

$(BIN_DIR)/gateway: $(wildcard cmd/gateway/*.go)
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $@ ./cmd/gateway

$(BIN_DIR)/mockupstream: $(wildcard cmd/mockupstream/*.go)
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $@ ./cmd/mockupstream
