.PHONY: help run build test tidy docker-up docker-down docker-app lint migrate-up migrate-down migrate-status migrate-version

help:
	@echo "Available targets:"
	@echo "  run              Run the server locally"
	@echo "  build            Build the server binary"
	@echo "  test             Run tests"
	@echo "  tidy             Tidy go modules"
	@echo "  docker-up        Start Postgres and Redis"
	@echo "  docker-down      Stop all Docker services"
	@echo "  docker-app       Start Postgres, Redis, and the app container"
	@echo "  migrate-up       Apply all pending migrations"
	@echo "  migrate-down     Roll back the last migration"
	@echo "  migrate-status   Show migration status"
	@echo "  migrate-version  Print current migration version"

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

test:
	go test ./...

tidy:
	go mod tidy

docker-up:
	docker compose up -d postgres redis

docker-down:
	docker compose --profile app down

docker-app:
	docker compose --profile app up -d --build

lint:
	golangci-lint run ./...

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status

migrate-version:
	go run ./cmd/migrate version
