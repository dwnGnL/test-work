package migrations_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

// Интеграционный тест миграции 0009: обходные пути вокруг журнала ledger. Нужен TEST_DATABASE_URL.
func TestLedgerHardening(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if err := postgres.Migrate(ctx, pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("entries cannot be appended to an existing transaction", func(t *testing.T) {
		if err := post(ctx, pool, "own:1", 100, -100); err != nil {
			t.Fatal(err)
		}
		// Повтор нашёл заголовок по ключу идемпотентности и дописывает вторую
		// сбалансированную пару: сумма по транзакции осталась бы нулём, зачисление — удвоено.
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			var txID int64
			if err := tx.QueryRow(ctx,
				`SELECT id FROM ledger.transactions WHERE idempotency_key = 'own:1'`).Scan(&txID); err != nil {
				return err
			}
			return addEntries(ctx, tx, txID, 100, -100)
		})
		assertCode(t, err, "23001") // restrict_violation
		if n := count(t, `
			SELECT count(*) FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
			WHERE t.idempotency_key = 'own:1'`); n != 2 {
			t.Errorf("entries stored = %d, want the original 2", n)
		}
	})

	t.Run("entries for a missing transaction are rejected", func(t *testing.T) {
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			return addEntries(ctx, tx, 987654321, 1, -1)
		})
		assertCode(t, err, "23001")
	})

	t.Run("entries in the transaction created by the same database transaction pass", func(t *testing.T) {
		if err := post(ctx, pool, "own:2", 10_000_000, -1_500_000, -8_500_000); err != nil {
			t.Fatalf("post: %v", err)
		}
	})

	t.Run("savepoints do not break the check", func(t *testing.T) {
		// pg_current_xact_id() отдаёт номер транзакции верхнего уровня, а не подтранзакции.
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			sp, err := tx.Begin(ctx) // SAVEPOINT: заголовок создаём в подтранзакции
			if err != nil {
				return err
			}
			var txID int64
			if err := sp.QueryRow(ctx,
				`INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ('own:3', 'test') RETURNING id`).Scan(&txID); err != nil {
				return err
			}
			if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
				return err
			}
			sp, err = tx.Begin(ctx) // проводки — в другой подтранзакции
			if err != nil {
				return err
			}
			if err := addEntries(ctx, sp, txID, 7, -7); err != nil {
				return err
			}
			return sp.Commit(ctx)
		})
		if err != nil {
			t.Fatalf("post with savepoints: %v", err)
		}
	})

	t.Run("truncate is forbidden on every ledger table", func(t *testing.T) {
		before := count(t, `SELECT count(*) FROM ledger.entries`)
		// У ledger.entries есть внешние ключи на две другие таблицы, поэтому для них нужен CASCADE:
		// без него Postgres откажет раньше триггера (0A000). Первой срабатывает защита именно
		// той таблицы, что названа в запросе, — по сообщению видно, что триггер есть у каждой.
		for _, table := range []string{"entries", "transactions", "accounts"} {
			_, err := pool.Exec(ctx, "TRUNCATE ledger."+table+" CASCADE")
			assertCode(t, err, "23001")
			if err != nil && !strings.Contains(err.Error(), "TRUNCATE on ledger."+table+" ") {
				t.Errorf("TRUNCATE ledger.%s was stopped by another table's trigger: %v", table, err)
			}
		}
		_, err := pool.Exec(ctx, `TRUNCATE ledger.entries, ledger.transactions, ledger.accounts`)
		assertCode(t, err, "23001")
		if after := count(t, `SELECT count(*) FROM ledger.entries`); after != before || before == 0 {
			t.Errorf("entries: %d before, %d after the attempts; journal must be intact", before, after)
		}
	})

	t.Run("accounts are append-only", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE ledger.accounts SET code = 'revenue:renamed' WHERE code = 'revenue:fees'`,
			`UPDATE ledger.accounts SET code = code`,
			`DELETE FROM ledger.accounts WHERE code = 'revenue:fees'`, // счёт без проводок: FK его не защитит
			`DELETE FROM ledger.accounts`,
		} {
			_, err := pool.Exec(ctx, stmt)
			if code := pgCode(err); code != "23001" {
				t.Errorf("%s: code = %q (err = %v), want 23001", stmt, code, err)
			}
		}
		if n := count(t, `SELECT count(*) FROM ledger.accounts WHERE code = 'revenue:fees'`); n != 1 {
			t.Errorf("account revenue:fees: count = %d, want 1", n)
		}
		// Добавлять счета по-прежнему можно.
		if _, err := pool.Exec(ctx, `INSERT INTO ledger.accounts (code) VALUES ('partner:hardening')`); err != nil {
			t.Errorf("insert account: %v", err)
		}
	})

	t.Run("transaction without entries is rejected on commit", func(t *testing.T) {
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ('empty:1', 'test')`)
			return err
		})
		assertCode(t, err, "23514")
		// Откат снял и заголовок: ключ идемпотентности не занят, повтор с проводками проходит.
		if n := count(t, `SELECT count(*) FROM ledger.transactions WHERE idempotency_key = 'empty:1'`); n != 0 {
			t.Errorf("header of the rejected transaction was stored (%d rows)", n)
		}
		if err := post(ctx, pool, "empty:1", 5, -5); err != nil {
			t.Errorf("retry with entries: %v", err)
		}
	})

	t.Run("transaction with a single entry is rejected on commit", func(t *testing.T) {
		err := post(ctx, pool, "single:1", 100)
		assertCode(t, err, "23514")
		// Одну проводку остановил бы и баланс, но отложенные проверки идут в порядке постановки,
		// а заголовок вставлен раньше проводки: первой срабатывает проверка числа проводок.
		if err != nil && !strings.Contains(err.Error(), "at least 2") {
			t.Errorf("rejected by another check: %v", err)
		}
	})
}

// addEntries дописывает проводки на два счёта (поочерёдно) в уже известную транзакцию ledger.
func addEntries(ctx context.Context, tx pgx.Tx, txID int64, amounts ...int64) error {
	for i, a := range amounts {
		code := "assets:deposits"
		if i%2 == 1 {
			code = "hold:aml"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger.entries (transaction_id, account_id, amount)
			SELECT $1, id, $2 FROM ledger.accounts WHERE code = $3`, txID, a, code); err != nil {
			return err
		}
	}
	return nil
}
