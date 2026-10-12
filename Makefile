.DEFAULT_GOAL := help

GO ?= go
TEST_DATABASE_URL ?= postgres://gateway:local-demo-password@127.0.0.1:54329/gateway?sslmode=disable
RUN ?=
export TEST_DATABASE_URL RUN

.PHONY: help postgres test-unit test-integration test test-scenarios vet check check-test-db

help:
	@printf '%s\n' \
	  'make postgres          — запустить локальный PostgreSQL, сохранив данные' \
	  'make test-unit         — unit-тесты без PostgreSQL и race detector' \
	  'make test-integration  — интеграционные тесты с PostgreSQL и -race' \
	  'make test              — все Go-тесты с PostgreSQL и -race' \
	  'make test-scenarios    — Bash-сценарии в отдельном Docker-project' \
	  'make vet               — статическая проверка Go' \
	  'make check             — все Go-тесты с -race и go vet' \
	  'RUN=TestName           — выбрать Go-тесты по регулярному выражению' \
	  'TEST_DATABASE_URL     — подключение; по умолчанию локальный Compose'

postgres:
	docker compose -f compose.yaml up -d postgres

test-unit:
	$(GO) test -count=1 -run "$$RUN" ./cmd/... ./internal/... ./migrations/...

check-test-db:
	@test -n "$$TEST_DATABASE_URL" || { printf '%s\n' 'TEST_DATABASE_URL не должен быть пустым: иначе интеграционные тесты будут пропущены.' >&2; exit 1; }

test-integration: check-test-db
	CGO_ENABLED=1 $(GO) test -race -count=1 -v -run "$$RUN" ./tests/integration

test: check-test-db
	CGO_ENABLED=1 $(GO) test -race -count=1 -run "$$RUN" ./...

test-scenarios:
	./scripts/run-all.sh

vet:
	$(GO) vet ./...

check: test vet
