package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
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

	want := embeddedVersions(t)
	if got := versions(t, pool); !slices.Equal(got, want) {
		t.Errorf("schema_migrations = %v, want %v", got, want)
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
	if want := len(embeddedVersions(t)); n != want {
		t.Errorf("applied %d migrations, want %d", n, want)
	}
	// Четыре системных счёта ledger вставляет миграция 0006: она выполнилась один раз.
	var accounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger.accounts`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 4 {
		t.Errorf("ledger accounts = %d, want the 4 system accounts", accounts)
	}
}

// База, где записана версия удалённой миграции (например, 0008_seed_demo, из которой убрали
// демо-данные), не должна мешать запуску: неизвестные применённые версии игнорируются.
func TestMigrateIgnoresUnknownAppliedVersions(t *testing.T) {
	ctx := context.Background()

	t.Run("embedded migrations", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		if err := postgres.Migrate(ctx, pool, migrations.FS, discardLog()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO public.schema_migrations (version) VALUES ('0008_seed_demo')`); err != nil {
			t.Fatal(err)
		}
		before := versions(t, pool)

		if err := postgres.Migrate(ctx, pool, migrations.FS, discardLog()); err != nil {
			t.Fatalf("Migrate with an unknown applied version: %v", err)
		}
		if after := versions(t, pool); !slices.Equal(after, before) {
			t.Errorf("schema_migrations changed: %v, was %v", after, before)
		}
	})

	t.Run("new migration is still applied", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		fsys := fstest.MapFS{"0001_a.sql": {Data: []byte(`CREATE TABLE a (id int);`)}}
		if err := postgres.Migrate(ctx, pool, fsys, discardLog()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO public.schema_migrations (version) VALUES ('0002_removed')`); err != nil {
			t.Fatal(err)
		}

		fsys["0003_c.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE c (id int);`)}
		if err := postgres.Migrate(ctx, pool, fsys, discardLog()); err != nil {
			t.Fatal(err)
		}
		if got := versions(t, pool); !slices.Equal(got, []string{"0001_a", "0002_removed", "0003_c"}) {
			t.Errorf("applied = %v, want [0001_a 0002_removed 0003_c]", got)
		}
	})
}

// Весь прогон — одна транзакция: сбой любой миграции откатывает и те, что в нём уже выполнились.
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
	if err == nil || !strings.Contains(err.Error(), "0002_b") {
		t.Fatalf("err = %v, want an error naming migration 0002_b", err)
	}
	// Откатилось всё: и сбойная 0002_b, и успешная 0001_a, и сама таблица учёта.
	for _, table := range []string{"public.a", "public.b", "public.schema_migrations"} {
		if exists(t, pool, table) {
			t.Errorf("table %s exists: the failed run was not rolled back as a whole", table)
		}
	}
	assertNoAdvisoryLocks(t, pool)

	// Чиним миграцию: следующий прогон применяет всё.
	fsys["0002_b.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id int);`)}
	if err := postgres.Migrate(ctx, pool, fsys, discardLog()); err != nil {
		t.Fatal(err)
	}
	if got := versions(t, pool); !slices.Equal(got, []string{"0001_a", "0002_b"}) {
		t.Errorf("applied = %v, want [0001_a 0002_b]", got)
	}

	// Версии, применённые прошлыми запусками, сбой нового прогона не трогает;
	// миграция нового прогона, выполненная до сбоя, откатывается вместе с ним.
	fsys["0003_c.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE c (id int);`)}
	fsys["0004_d.sql"] = &fstest.MapFile{Data: []byte(`SELECT 1/0;`)}
	if err := postgres.Migrate(ctx, pool, fsys, discardLog()); err == nil {
		t.Fatal("expected error from the failing migration")
	}
	if got := versions(t, pool); !slices.Equal(got, []string{"0001_a", "0002_b"}) {
		t.Errorf("after the second failure applied = %v, want [0001_a 0002_b]", got)
	}
	if exists(t, pool, "public.c") {
		t.Error("table c exists: 0003_c should have been rolled back with the failed run")
	}
	assertNoAdvisoryLocks(t, pool)
}

// Блокировка транзакционная: после успешного и после неудачного прогона никто её не держит.
func TestMigrateReleasesLock(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	if err := postgres.Migrate(ctx, pool, migrations.FS, discardLog()); err != nil {
		t.Fatal(err)
	}
	assertNoAdvisoryLocks(t, pool)

	// Отменённый контекст: прогон падает, блокировка и соединение не остаются за пулом.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := postgres.Migrate(cctx, pool, migrations.FS, discardLog()); err == nil {
		t.Error("expected error for cancelled context")
	}
	assertNoAdvisoryLocks(t, pool)
	assertNoOpenTransaction(t, pool)
}

// assertNoAdvisoryLocks проверяет, что в текущей базе нет advisory-блокировок.
func assertNoAdvisoryLocks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory'
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d advisory locks are still held", n)
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

// embeddedVersions — версии вшитых миграций по порядку имён. Число миграций растёт,
// поэтому тесты сверяются с каталогом, а не с константой.
func embeddedVersions(t *testing.T) []string {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		files[i] = strings.TrimSuffix(f, ".sql")
	}
	return files // Glob отдаёт имена отсортированными
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
