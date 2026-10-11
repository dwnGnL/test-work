package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

// CheckBalanced молчит на журнале, который вёлся через Post, и сообщает об ошибке,
// если журнал испорчен в обход триггеров.
func TestCheckBalanced(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)

	if err := ledger.CheckBalanced(ctx, pool); err != nil {
		t.Fatalf("empty journal: %v", err)
	}
	mustPost(t, pool, holdPosting(t, "p1:1"))
	mustPost(t, pool, refundPosting(t, "p1:2"))
	if err := ledger.CheckBalanced(ctx, pool); err != nil {
		t.Fatalf("after postings: %v", err)
	}

	// Одна проводка без пары. Обычным путём база её не примет, поэтому на время
	// транзакции отключаем триггеры (нужны права суперпользователя, в тестовой базе они есть).
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			WITH t AS (
				INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ('broken', 'test') RETURNING id
			)
			INSERT INTO ledger.entries (transaction_id, account_id, amount)
			SELECT t.id, a.id, 1 FROM t, ledger.accounts a WHERE a.code = $1`, ledger.AccountDeposits)
		return err
	})
	if err != nil {
		t.Fatalf("break the journal: %v", err)
	}

	if err := ledger.CheckBalanced(ctx, pool); !errors.Is(err, ledger.ErrBooksUnbalanced) {
		t.Fatalf("broken journal: err = %v, want ErrBooksUnbalanced", err)
	}
}
