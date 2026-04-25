BINARY   := bitmark-hp-gen
CMD      := ./cmd/bitmark-hp-gen

# Web-root and template target dirs (override via env if your domain differs)
EXPLORER_DOMAIN ?= your-explorer-domain.example.com
OUTPUT_DIR      ?= /var/www/$(EXPLORER_DOMAIN)/html
TEMPLATE_DIR    ?= /var/www/templates/bitmark-hp-gen

.PHONY: build run vet tidy clean install deploy-assets

## build: compile the binary into the repo root (CGo required for zmq4/goncurses)
build:
	CGO_ENABLED=1 go build -o $(BINARY) $(CMD)

## run: build and run locally (requires PG_DSN and other env vars to be set)
run: build
	./$(BINARY)

## vet: run go vet across all packages
vet:
	go vet ./...

## tidy: tidy go.mod / go.sum
tidy:
	go mod tidy

## clean: remove the local binary
clean:
	rm -f $(BINARY)

## deploy-assets: copy templates and static files without a full reinstall (requires root)
deploy-assets:
	mkdir -p $(TEMPLATE_DIR) $(OUTPUT_DIR)
	cp templates/*.html $(TEMPLATE_DIR)/
	cp templates/*.png templates/*.svg templates/*.ico templates/*.css templates/*.js $(OUTPUT_DIR)/ 2>/dev/null || true

## install: full system install (requires root — delegates to install.sh)
install:
	sudo ./install.sh
