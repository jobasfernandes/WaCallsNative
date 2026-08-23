COMPOSE ?= docker compose
POSTGRES_COMPOSE ?= $(COMPOSE) -f docker-compose.yml -f docker-compose.postgres.yml

# Load .env into recipe environments for the local dev targets (ignored if absent).
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: build up up-postgres down restart logs sh clean dev dev-logs server client test lint tidy

build:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d

up-postgres:
	$(POSTGRES_COMPOSE) up -d

down:
	$(COMPOSE) down

restart:
	$(COMPOSE) up -d --force-recreate

logs:
	$(COMPOSE) logs -f wacalls

sh:
	$(COMPOSE) exec wacalls sh

clean:
	$(COMPOSE) down -v

# --- Local development (hot reload) ---
# `make dev` runs the Go server under air (rebuild+restart on .go changes) on :3001
# and the Vite client on :5173 (which proxies /api -> :3001). Open http://localhost:5173.
# Dev logs are mirrored to files so a session can be inspected after the fact:
# the terminal keeps showing everything, and DEV_LOG_DIR keeps a copy. It is not
# the air tmp_dir on purpose, because air wipes that on exit.
DEV_LOG_DIR ?= .devlogs

dev:
	$(MAKE) -j2 server client

server:
	@mkdir -p $(DEV_LOG_DIR)
	air 2>&1 | tee $(DEV_LOG_DIR)/server.log

client:
	@mkdir -p $(DEV_LOG_DIR)
	cd client && npm run dev 2>&1 | tee ../$(DEV_LOG_DIR)/client.log

# dev-logs prints where the mirrored logs are, for pointing a tool at them.
dev-logs:
	@ls -la $(DEV_LOG_DIR) 2>/dev/null || echo "no dev logs yet; run make dev first"

test:
	go test ./... -count=1

lint:
	golangci-lint run ./...

tidy:
	go mod tidy
