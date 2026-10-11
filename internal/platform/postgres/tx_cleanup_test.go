package postgres_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Все пути выхода из InTx, кроме успешного Commit, обязаны откатить транзакцию и вернуть
// соединение в пул: ошибка fn, panic, runtime.Goexit, отмена контекста внутри fn.
func TestInTxAlwaysReleasesTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE items (v int)`); err != nil {
		t.Fatal(err)
	}
	insert := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO items VALUES (1)`)
		return err
	}

	// track запоминает транзакцию, открытую InTx: см. очистку в цикле ниже.
	tests := []struct {
		name string
		run  func(t *testing.T, track func(pgx.Tx))
	}{
		{"error from fn", func(t *testing.T, track func(pgx.Tx)) {
			sentinel := errors.New("boom")
			err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
				track(tx)
				if err := insert(tx); err != nil {
					return err
				}
				return sentinel
			})
			if err != sentinel { //nolint:errorlint // ошибка не должна быть обёрнута
				t.Errorf("err = %v, want exactly the sentinel", err)
			}
		}},
		{"panic", func(t *testing.T, track func(pgx.Tx)) {
			defer func() {
				if r := recover(); r != "oops" {
					t.Errorf("recovered %v, want oops", r)
				}
			}()
			_ = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
				track(tx)
				if err := insert(tx); err != nil {
					return err
				}
				panic("oops")
			})
		}},
		{"runtime.Goexit", func(t *testing.T, track func(pgx.Tx)) {
			// Goexit завершает горутину, не вызывая recover; откат обязан сработать из defer.
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
					track(tx)
					if err := insert(tx); err != nil {
						t.Errorf("insert: %v", err)
						return err
					}
					runtime.Goexit()
					return nil
				})
				t.Error("InTx returned after runtime.Goexit")
			}()
			<-done
		}},
		{"context cancelled inside fn", func(t *testing.T, track func(pgx.Tx)) {
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			err := postgres.InTx(cctx, pool, func(tx pgx.Tx) error {
				track(tx)
				if _, err := tx.Exec(cctx, `INSERT INTO items VALUES (1)`); err != nil {
					return err
				}
				cancel()
				return cctx.Err()
			})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opened pgx.Tx
			// Если InTx не откатил транзакцию, соединение не вернётся в пул, и pool.Close()
			// при удалении тестовой базы повиснет. Откатываем сами: тест должен упасть, а не зависнуть.
			t.Cleanup(func() {
				if opened != nil {
					_ = opened.Rollback(context.Background())
				}
			})
			tt.run(t, func(tx pgx.Tx) { opened = tx })

			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil || n != 0 {
				t.Errorf("rows = %d (err = %v), want 0: transaction was not rolled back", n, err)
			}
			assertNoOpenTransaction(t, pool)
		})
	}
}

// assertNoOpenTransaction проверяет, что ни одно соединение не осталось внутри незавершённой
// транзакции и все соединения вернулись в пул.
func assertNoOpenTransaction(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections are still acquired from the pool", n)
	}
	var idle int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&idle)
	if err != nil {
		t.Fatal(err)
	}
	if idle != 0 {
		t.Errorf("%d sessions are left 'idle in transaction'", idle)
	}
}
