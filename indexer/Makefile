BINARY := bitmark-indexer
CMD    := ./cmd/bitmark-indexer
GO     := /usr/local/go/bin/go   # Go 1.25 — system `go` may be older

export GOPATH     ?= /root/go
export GOMODCACHE ?= $(GOPATH)/pkg/mod
export GOCACHE    ?= /root/.cache/go-build

.PHONY: build install restart clean

## build: compile the binary into the repo root
build:
	CGO_ENABLED=1 $(GO) build -o $(BINARY) $(CMD)

## install: build, atomically swap the binary, then restart the service
install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
	systemctl restart $(BINARY)
	@sleep 2 && systemctl is-active $(BINARY)

## restart: restart the systemd service without rebuilding
restart:
	systemctl restart $(BINARY)

## clean: remove local build artifact
clean:
	rm -f $(BINARY)
