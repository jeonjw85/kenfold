VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/kenfold/kenfold/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

TEST_DB     := kenfold_test
TEST_DB_URL := postgres://kenfold:$${KENFOLD_DB_PASSWORD:-kenfold}@127.0.0.1:54329/$(TEST_DB)?sslmode=disable

EMBED_MODEL     ?= bge-m3
CHAT_MODEL      ?= qwen3.5:4b
EXTRACT_MODEL   ?= kenfold-extract
BUILD_ENV   := VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE)
# Runs the kenfold binary inside the running container (it has the database URL).
KENFOLD_CLI := docker compose exec -T kenfold /usr/local/bin/kenfold

# tree-sitter grammars embedded in bin/kenfold, used to find the symbols
# memories refer to (`kenfold refs sync` and the session hook). Plain
# `go build` embeds all ~200 grammars (about 15 MB more).
GRAMMARS  ?= go typescript tsx javascript python rust java kotlin ruby c_sharp php c cpp swift scala bash
GO_TAGS   := grammar_subset $(addprefix grammar_subset_,$(GRAMMARS))

.PHONY: build test test-integration lint fmt up up-embed up-extract down logs migrate key keys reindex review eval eval-search clean

build: ## Build ./bin/kenfold
	CGO_ENABLED=0 go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/kenfold ./cmd/kenfold

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

# The compose Ollama runs on CPU with one thread per CPU by default. On CPUs
# with performance and efficiency cores (e.g. Apple Silicon) that is far
# slower: measured on an M5, bge-m3 took 3.3 s per embedding instead of 0.1 s,
# and qwen3.5:4b generated 0.3 instead of ~25 tokens/s. The derived models only
# pin the thread count (outputs are identical); set THREADS to your number of
# performance cores. KENFOLD_EMBED_NAME keeps recording vectors as bge-m3.
THREADS ?= 4
derive = docker compose exec -T ollama sh -c 'printf "FROM %s\nPARAMETER num_thread %s\n%b" "$(1)" "$(THREADS)" "$(3)" > /tmp/Modelfile && ollama create $(2) -f /tmp/Modelfile'

# The reranker (llama.cpp, bge-reranker-v2-m3) raised recall@5 on the internal
# eval set from 0.90 to 0.96 and adds 1–2 s per search on CPU. RERANK=0 leaves it out.
RERANK ?= 1
ifeq ($(RERANK),1)
SEARCH_PROFILES := --profile embed --profile rerank
SEARCH_SERVICES := ollama reranker
SEARCH_ENV      := KENFOLD_RERANK_URL=http://reranker:8080/v1
else
SEARCH_PROFILES := --profile embed
SEARCH_SERVICES := ollama
SEARCH_ENV      :=
endif
EMBED_ENV := KENFOLD_EMBED_URL=http://ollama:11434/v1 KENFOLD_EMBED_MODEL=kenfold-embed KENFOLD_EMBED_NAME=$(EMBED_MODEL)

up-embed: ## Start Postgres + Ollama (+ reranker) + Kenfold with hybrid search (first run downloads ~1.9 GB)
	THREADS=$(THREADS) docker compose $(SEARCH_PROFILES) up -d --wait postgres $(SEARCH_SERVICES)
	docker compose exec -T ollama ollama pull $(EMBED_MODEL)
	$(call derive,$(EMBED_MODEL),kenfold-embed,)
	$(BUILD_ENV) THREADS=$(THREADS) $(EMBED_ENV) $(SEARCH_ENV) \
	  docker compose $(SEARCH_PROFILES) up -d --build --wait

up-extract: ## up-embed + a local chat model for memory extraction (first run downloads qwen3.5:4b, ~3.4 GB)
	THREADS=$(THREADS) docker compose $(SEARCH_PROFILES) up -d --wait postgres $(SEARCH_SERVICES)
	docker compose exec -T ollama ollama pull $(EMBED_MODEL)
	docker compose exec -T ollama ollama pull $(CHAT_MODEL)
	$(call derive,$(EMBED_MODEL),kenfold-embed,)
	$(call derive,$(CHAT_MODEL),$(EXTRACT_MODEL),PARAMETER num_ctx 8192\nPARAMETER temperature 0\n)
	$(BUILD_ENV) THREADS=$(THREADS) $(EMBED_ENV) $(SEARCH_ENV) \
	  KENFOLD_CHAT_URL=http://ollama:11434/v1 KENFOLD_CHAT_MODEL=$(EXTRACT_MODEL) \
	  docker compose $(SEARCH_PROFILES) up -d --build --wait

down: ## Stop the stack, including the optional model services (data volumes are kept)
	docker compose --profile embed --profile rerank down

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

review: ## Review proposed memories (extracted memories, preferences) interactively
	docker compose exec kenfold /usr/local/bin/kenfold memory review

eval: ## Measure extraction and classification on the internal eval set (needs `make up-extract`)
	KENFOLD_EVAL_CHAT_URL=http://127.0.0.1:11435/v1 KENFOLD_EVAL_CHAT_MODEL=$(EXTRACT_MODEL) \
	  go test -count=1 -run TestEvalModel -v -timeout 30m ./internal/extract/

eval-search: ## Measure retrieval (recall@5 per pipeline stage) on the internal corpus (needs `make up-embed`)
	@docker compose exec -T postgres psql -U kenfold -d kenfold -tAc \
	  "SELECT 1 FROM pg_database WHERE datname = '$(TEST_DB)'" | grep -q 1 || \
	  docker compose exec -T postgres psql -U kenfold -d kenfold -c "CREATE DATABASE $(TEST_DB)"
	KENFOLD_TEST_DATABASE_URL="$(TEST_DB_URL)" KENFOLD_EVAL_EMBED_URL=http://127.0.0.1:11435/v1 KENFOLD_EVAL_EMBED_MODEL=kenfold-embed \
	  $(if $(filter 1,$(RERANK)),KENFOLD_EVAL_RERANK_URL=http://127.0.0.1:11436/v1) \
	  go test -count=1 -run TestEvalRetrieval -v -timeout 30m ./internal/retrieve/

clean:
	rm -rf bin
