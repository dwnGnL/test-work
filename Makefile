.PHONY: build test vet up down

build:
	go build -o bin/server ./cmd/server

# Интеграционные тесты ходят в Postgres из docker compose (по умолчанию порт 55432 на localhost).
# Каждый тест создаёт себе временную базу, основную не трогает.
# Порт задаёт DB_PORT, тот же, что читает docker-compose.yml: export передаёт его и docker compose.
# Тесты на втором стеке: make test DB_PORT=55433 COMPOSE_PROJECT_NAME=clean.
export DB_PORT ?= 55432
TEST_DATABASE_URL ?= postgres://payments:payments@127.0.0.1:$(DB_PORT)/payments?sslmode=disable

test:
	docker compose up -d --wait db
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

vet:
	go vet ./...

up:
	docker compose up --build

down:
	docker compose down
