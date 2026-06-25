COMPOSE ?= docker compose

.PHONY: build up down restart logs sh clean

build:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d

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
