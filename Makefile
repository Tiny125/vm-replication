# vm-replication — build and test targets.
# The binaries are static (CGO disabled) so they drop cleanly onto any Linux
# source server and into Linode's Finnix rescue environment.

GO        ?= go
BIN       ?= bin
LDFLAGS   ?= -s -w
GOFLAGS   ?=
CGO_ENABLED ?= 0

.PHONY: all build agent receiver controld replctl applianced test test-scripts vet smoke certs clean

all: build

build: agent receiver controld replctl applianced

# The turnkey "replication server" daemon (web console + enrollment + finalize).
applianced:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/applianced ./cmd/applianced

agent:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/agent ./cmd/agent

receiver:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/receiver ./cmd/receiver

# controld links the pure-Go SQLite driver; still static (CGO disabled).
controld:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/controld ./cmd/controld

replctl:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/replctl ./cmd/replctl

test: test-scripts
	$(GO) test ./...

# Shell-level unit tests for the conversion helpers (no root / block devices).
test-scripts:
	bash scripts/machine-convert-test.sh

vet:
	$(GO) vet ./...

# End-to-end proof using file-backed images on this host.
smoke:
	bash scripts/smoke-test.sh

# Dev mTLS material in ./certs (override SAN: make certs SAN=203.0.113.10).
SAN ?= localhost
certs:
	bash scripts/gen-certs.sh certs $(SAN)

clean:
	rm -rf $(BIN) certs *.cbt *.img
