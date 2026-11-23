.PHONY: help install update upgrade dev kill build fake test check ci e2e bruno e2e-ci docker clean

-include .env

BRU ?= npx --yes @usebruno/cli@4

help:
	@echo "Targets:"
	@echo "  install      Download Go module dependencies"
	@echo "  update       Refresh module dependencies within go.mod constraints"
	@echo "  upgrade      Upgrade module dependencies to latest"
	@echo "  dev          Run the API server against the real claude CLI (.env)"
	@echo "  kill         Kill the API server (PORT from .env)"
	@echo "  build        Build bin/creditapi"
	@echo "  fake         Build bin/fakeclaude (fake Claude CLI for tests and Bruno)"
	@echo "  test         Run Go unit and integration tests"
	@echo "  check        tidy, format, vet, and test"
	@echo "  ci           Check-only: format diff, vet, and race tests"
	@echo "  e2e          Run the server with the fake Claude CLI for Bruno (.env.bruno)"
	@echo "  bruno        Run the Bruno collection against a running e2e stack"
	@echo "  e2e-ci       Start the e2e stack, run Bruno headless, stop it (used by CI)"
	@echo "  docker       Build the Docker image"
	@echo "  clean        Remove build artifacts and e2e data"

install:
	go mod download

update:
	go get ./...
	go mod tidy

upgrade:
	go get -u ./...
	go mod tidy

dev: install kill
	go run ./src/cmd/creditapi

kill:
	fuser -k $(or $(PORT),8080)/tcp 2>/dev/null || true

build:
	go build -o bin/creditapi ./src/cmd/creditapi

fake:
	go build -o bin/fakeclaude ./src/cmd/fakeclaude

test:
	go test ./...

check:
	go mod tidy
	gofmt -w src
	go vet ./...
	$(MAKE) test

ci:
	@test -z "$$(gofmt -l src)" || (echo 'gofmt needed:' && gofmt -l src && exit 1)
	go vet ./...
	go test -race ./...

e2e: fake
	@test -f .env.bruno || (echo 'copy .env.bruno.example to .env.bruno' >&2; exit 1)
	rm -rf ./data/bruno
	ENV_CREDITAPI=$(abspath .env.bruno) go run ./src/cmd/creditapi

bruno:
	cd bruno && $(BRU) run --env local

e2e-ci:
	BRU="$(BRU)" ./scripts/e2e.sh

docker:
	docker build -t creditapi .

clean:
	rm -rf bin/ data/bruno/ bruno/results/
	go clean -cache -testcache 2>/dev/null || true
