// Package pgtest создаёт изолированную базу данных для интеграционных тестов.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL — переменная окружения с адресом Postgres, на котором тесты могут создавать базы
// (нужна роль с правом CREATEDB), например postgres://user:pass@localhost:5432/postgres.
const EnvURL = "TEST_DATABASE_URL"

// NewPool создаёт на сервере из TEST_DATABASE_URL пустую базу test_<случайный суффикс>
// и возвращает пул к ней. База удаляется по завершении теста. Если переменная не
// задана, тест пропускается: интеграционные тесты не должны ронять обычный go test.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("%s is not set: skipping integration test", EnvURL)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	name := "test_" + hex.EncodeToString(suffix)
	quoted := pgx.Identifier{name}.Sanitize()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminExec(ctx, t, url, "CREATE DATABASE "+quoted)

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		// FORCE обрывает сессии, которые тест мог оставить открытыми.
		adminExec(cctx, t, url, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	return pool
}

// adminExec выполняет оператор отдельным соединением к базе из url.
func adminExec(ctx context.Context, t testing.TB, url, sql string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
