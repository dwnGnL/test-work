.PHONY: build test vet up down

build:
	go build -o bin/server ./cmd/server

# Интеграционные тесты ходят в Postgres из docker compose (порт 55432 на localhost).
# Каждый тест создаёт себе временную базу, основную не трогает.
TEST_DATABASE_URL ?= postgres://payments:payments@127.0.0.1:55432/payments?sslmode=disable

test:
	docker compose up -d --wait db
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

vet:
	go vet ./...

up:
	docker compose up --build

down:
	docker compose down
