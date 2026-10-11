package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestMigrateAppliesAllAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	for i := range 2 { // второй запуск ничего не должен менять
		if err := postgres.Migrate(ctx, pool, migrations.FS, discardLog()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	var n int
	var first, last string
	err := pool.QueryRow(ctx,
		`SELECT count(*), min(version), max(version) FROM public.schema_migrations`).Scan(&n, &first, &last)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 || first != "0001_partners" || last != "0008_seed_demo" {
		t.Errorf("schema_migrations: count=%d first=%q last=%q", n, first, last)
	}
}

// Два инстанса стартуют одновременно: без advisory lock проигравший упал бы на
// «schema already exists». Здесь все запуски обязаны завершиться без ошибок.
func TestMigrateConcurrentRunners(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	const runners = 6
	var wg sync.WaitGroup
	errs := make([]error, runners)
	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = postgres.Migrate(ctx, pool, migrations.FS, discardLog())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("applied %d migrations, want 8", n)
	}
	var partners int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM partners.partners`).Scan(&partners); err != nil {
		t.Fatal(err)
	}
	if partners != 2 {
		t.Errorf("seed ran %d times, want exactly once (2 partners), got %d rows", partners/2, partners)
	}
}

func TestMigrateOrderAndRollback(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	fsys := fstest.MapFS{
		"0002_b.sql":    {Data: []byte(`CREATE TABLE b (id int); SELECT 1/0;`)}, // сбой после CREATE TABLE
		"0001_a.sql":    {Data: []byte(`CREATE TABLE a (id int);`)},
		"README.md":     {Data: []byte(`not a migration`)},
		"sub/0003.sql":  {Data: []byte(`this is in a subdirectory and must be ignored`)},
		"notes.sql.bak": {Data: []byte(`ignored`)},
	}

	err := postgres.Migrate(ctx, pool, fsys, discardLog())
	if err == nil {
		t.Fatal("expected error from the failing migration")
	}
	if got := versions(t, pool); len(got) != 1 || got[0] != "0001_a" {
		t.Errorf("after failure applied = %v, want [0001_a]", got)
	}
	if exists(t, pool, "public.b") {
		t.Error("table b exists: failed migration was not rolled back")
	}

	// Чиним миграцию: раннер добирает только недостающую.
	fsys["0002_b.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id int);`)}
	if err := postgres.Migrate(ctx, pool, fsys, discardLog()); err != nil {
		t.Fatal(err)
	}
	if got := versions(t, pool); len(got) != 2 || got[1] != "0002_b" {
		t.Errorf("applied = %v, want [0001_a 0002_b]", got)
	}
}

func TestInTx(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE items (v int)`); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO items VALUES (1)`)
		return err
	}

	t.Run("commit", func(t *testing.T) {
		if err := postgres.InTx(ctx, pool, insert); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != 1 {
			t.Errorf("rows = %d, want 1", n)
		}
	})

	t.Run("rollback on error, error returned as is", func(t *testing.T) {
		before := count()
		sentinel := errors.New("boom")
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			if err := insert(tx); err != nil {
				return err
			}
			return sentinel
		})
		if err != sentinel { //nolint:errorlint // проверяем, что ошибка не обёрнута
			t.Errorf("err = %v, want exactly the sentinel", err)
		}
		if n := count(); n != before {
			t.Errorf("rows = %d, want %d: transaction was not rolled back", n, before)
		}
	})

	t.Run("rollback on panic, panic propagates", func(t *testing.T) {
		before := count()
		func() {
			defer func() {
				if r := recover(); r != "oops" {
					t.Errorf("recovered %v, want oops", r)
				}
			}()
			_ = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
				if err := insert(tx); err != nil {
					return err
				}
				panic("oops")
			})
		}()
		if n := count(); n != before {
			t.Errorf("rows = %d, want %d: transaction was not rolled back", n, before)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := postgres.InTx(cctx, pool, insert); err == nil {
			t.Error("expected error for cancelled context")
		}
	})
}

func versions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT version FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1)::text`, table).Scan(&reg); err != nil {
		t.Fatal(err)
	}
	return reg != nil
}
