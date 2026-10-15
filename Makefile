# Tools are pinned in tools/go.mod and run through `go tool`, nothing to install globally.
TOOL    := go tool -modfile=tools/go.mod
COMPOSE := docker compose -f deploy/docker-compose.yml
GOOSE   := go tool -modfile=tools/go.mod goose -dir migrations postgres

-include .env
export

.PHONY: help up down logs run build test lint fmt migrate migrate-down migrate-new sqlc check

help: ## Show available targets
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

up: ## Start PostgreSQL, MinIO and LiveKit
	$(COMPOSE) up -d --wait

down: ## Stop local infrastructure
	$(COMPOSE) down

logs: ## Follow infrastructure logs
	$(COMPOSE) logs -f

run: ## Run the API on the host
	go run ./cmd/orbit

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -o bin/orbit ./cmd/orbit

test: ## Run tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint
	$(TOOL) golangci-lint run ./...

fmt: ## Format code
	$(TOOL) golangci-lint fmt ./...

migrate: ## Apply migrations
	$(GOOSE) "$(ORBIT_DATABASE_URL)" up

migrate-down: ## Roll back the last migration
	$(GOOSE) "$(ORBIT_DATABASE_URL)" down

migrate-new: ## Create a migration: make migrate-new name=create_users
	$(GOOSE) "" create $(name) sql

sqlc: ## Generate Go code from db/queries
	$(TOOL) sqlc generate

check: lint test ## Everything CI runs
