VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/kenfold/kenfold/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

TEST_DB     := kenfold_test
TEST_DB_URL := postgres://kenfold:$${KENFOLD_DB_PASSWORD:-kenfold}@127.0.0.1:54329/$(TEST_DB)?sslmode=disable

.PHONY: build test test-integration lint fmt up down logs migrate clean

build: ## Build ./bin/kenfold
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kenfold ./cmd/kenfold

test: ## Unit tests + stdio end-to-end test
	go test ./...

test-integration: ## Integration tests against the compose Postgres (throwaway database)
	@docker compose exec -T postgres psql -U kenfold -d kenfold -tAc \
	  "SELECT 1 FROM pg_database WHERE datname = '$(TEST_DB)'" | grep -q 1 || \
	  docker compose exec -T postgres psql -U kenfold -d kenfold -c "CREATE DATABASE $(TEST_DB)"
	KENFOLD_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -count=1 -run Integration -v ./migrations/ ./internal/store/

lint: ## gofmt check + go vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)
	go vet ./...

fmt:
	gofmt -w .

up: ## Start Postgres + Kenfold (builds the image)
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) docker compose up -d --build --wait

down: ## Stop the stack (data volume is kept)
	docker compose down

logs:
	docker compose logs -f kenfold

migrate: build ## Apply migrations to the compose database from the host
	./bin/kenfold migrate up

clean:
	rm -rf bin
