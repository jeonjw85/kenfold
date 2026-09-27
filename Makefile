VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/kenfold/kenfold/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

TEST_DB     := kenfold_test
TEST_DB_URL := postgres://kenfold:$${KENFOLD_DB_PASSWORD:-kenfold}@127.0.0.1:54329/$(TEST_DB)?sslmode=disable

EMBED_MODEL ?= bge-m3
BUILD_ENV   := VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE)
# Runs the kenfold binary inside the running container (it has the database URL).
KENFOLD_CLI := docker compose exec -T kenfold /usr/local/bin/kenfold

.PHONY: build test test-integration lint fmt up up-embed down logs migrate key keys reindex clean

build: ## Build ./bin/kenfold
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kenfold ./cmd/kenfold

test: ## Unit tests + stdio end-to-end test (no database needed)
	go test ./...

test-integration: ## Integration tests against the compose Postgres (throwaway database, run `make up` first)
	@docker compose exec -T postgres psql -U kenfold -d kenfold -tAc \
	  "SELECT 1 FROM pg_database WHERE datname = '$(TEST_DB)'" | grep -q 1 || \
	  docker compose exec -T postgres psql -U kenfold -d kenfold -c "CREATE DATABASE $(TEST_DB)"
	KENFOLD_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -count=1 -p 1 -run Integration -v ./...

lint: ## gofmt check + go vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)
	go vet ./...

fmt:
	gofmt -w .

up: ## Start Postgres + Kenfold with full-text search (builds the image)
	$(BUILD_ENV) docker compose up -d --build --wait

up-embed: ## Start Postgres + Ollama + Kenfold with hybrid search (first run downloads bge-m3, ~1.2 GB)
	docker compose --profile embed up -d --wait postgres ollama
	docker compose exec -T ollama ollama pull $(EMBED_MODEL)
	$(BUILD_ENV) KENFOLD_EMBED_URL=http://ollama:11434/v1 KENFOLD_EMBED_MODEL=$(EMBED_MODEL) \
	  docker compose --profile embed up -d --build --wait

down: ## Stop the stack, including Ollama (data volumes are kept)
	docker compose --profile embed down

logs:
	docker compose logs -f kenfold

migrate: build ## Apply migrations to the compose database from the host
	./bin/kenfold migrate up

key: ## Create an API key for an agent: make key AGENT=claude-code
	@test -n "$(AGENT)" || { echo "usage: make key AGENT=<name>   (e.g. claude-code, codex, opencode)" >&2; exit 2; }
	@$(KENFOLD_CLI) key create $(AGENT)

keys: ## List API keys
	@$(KENFOLD_CLI) key list

reindex: ## Embed memories that have no embedding for the configured model
	$(KENFOLD_CLI) reindex

clean:
	rm -rf bin
