package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dwnGnL/test-work/internal/ledger"
)

// EnsureAccount идемпотентен: повтор, уже существующий системный счёт и параллельные
// вызовы (каждый в своей транзакции) не дают ошибки и не создают дубликатов.
func TestEnsureAccount(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	accounts := func(code string) int {
		return count(t, pool, `SELECT count(*) FROM ledger.accounts WHERE code = $1`, code)
	}

	t.Run("repeat", func(t *testing.T) {
		if n := accounts("partner:101"); n != 0 {
			t.Fatalf("partner:101 exists before the test (%d rows)", n)
		}
		for range 2 {
			if err := ensure(ctx, pool, ledger.PartnerAccount(101)); err != nil {
				t.Fatal(err)
			}
		}
		if n := accounts("partner:101"); n != 1 {
			t.Errorf("partner:101 rows = %d, want 1", n)
		}
	})

	t.Run("system account", func(t *testing.T) {
		if err := ensure(ctx, pool, ledger.AccountRevenueFees); err != nil {
			t.Fatal(err)
		}
		if n := accounts(ledger.AccountRevenueFees); n != 1 {
			t.Errorf("%s rows = %d, want 1", ledger.AccountRevenueFees, n)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		if n := accounts("partner:102"); n != 0 {
			t.Fatalf("partner:102 exists before the test (%d rows)", n)
		}
		warmUp(t, pool)
		const workers = 20
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = ensure(ctx, pool, ledger.PartnerAccount(102))
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}
		if n := accounts("partner:102"); n != 1 {
			t.Errorf("partner:102 rows = %d, want 1", n)
		}
	})
}

// PartnerBalance после зачисления — положительное число (сальдо счёта-обязательства
// отрицательное, знак меняется). Чтение работает и на пуле, и на транзакции.
func TestPartnerBalance(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	if err := ensure(ctx, pool, ledger.PartnerAccount(7)); err != nil {
		t.Fatal(err)
	}

	got, err := ledger.PartnerBalance(ctx, pool, 7)
	if err != nil || got != 0 {
		t.Fatalf("before credit: balance = %v, err = %v; want 0", got, err)
	}

	mustPost(t, pool, holdPosting(t, "p1:1"))
	mustPost(t, pool, creditPosting(t, "p1:2", 7))

	got, err = ledger.PartnerBalance(ctx, pool, 7)
	if err != nil || got != amt(t, "98.5") {
		t.Errorf("after credit: balance = %v, err = %v; want 98.500000", got, err)
	}
	// Сальдо самого счёта — со знаком обязательства.
	signed, err := ledger.Balance(ctx, pool, ledger.PartnerAccount(7))
	if err != nil || signed != -amt(t, "98.5") {
		t.Errorf("Balance = %v, err = %v; want -98.500000", signed, err)
	}

	t.Run("read through transaction", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if posted, err := ledger.Post(ctx, tx, holdPosting(t, "p2:1")); !posted || err != nil {
			t.Fatalf("Post = %v, %v", posted, err)
		}
		if posted, err := ledger.Post(ctx, tx, creditPosting(t, "p2:2", 7)); !posted || err != nil {
			t.Fatalf("Post = %v, %v", posted, err)
		}
		inTx, err := ledger.PartnerBalance(ctx, tx, 7)
		if err != nil || inTx != amt(t, "197") {
			t.Errorf("in transaction: balance = %v, err = %v; want 197.000000", inTx, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		afterRollback, err := ledger.PartnerBalance(ctx, pool, 7)
		if err != nil || afterRollback != amt(t, "98.5") {
			t.Errorf("after rollback: balance = %v, err = %v; want 98.500000", afterRollback, err)
		}
	})
}

// Несуществующий счёт — явная ошибка, а не нулевой баланс.
func TestBalanceUnknownAccount(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)

	if _, err := ledger.Balance(ctx, pool, "partner:404"); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("Balance: err = %v, want ErrUnknownAccount", err)
	}
	if _, err := ledger.PartnerBalance(ctx, pool, 404); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("PartnerBalance: err = %v, want ErrUnknownAccount", err)
	}
	// Счёт без проводок существует и даёт честный ноль.
	if got, err := ledger.Balance(ctx, pool, ledger.AccountRefundsPayable); err != nil || got != 0 {
		t.Errorf("empty account: balance = %v, err = %v; want 0", got, err)
	}
}
