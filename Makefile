GO ?= go
NPM ?= npm

.PHONY: web build test dev compose-check

web:
	cd web && $(NPM) ci && $(NPM) run build

build: web
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -o dist/upfile ./cmd/upfile

test: web
	cd web && $(NPM) test
	$(GO) test ./...

dev: web
	$(GO) run ./cmd/dev

compose-check:
	docker compose config --quiet
