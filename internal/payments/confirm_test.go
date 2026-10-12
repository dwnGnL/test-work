package payments_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

const confirmations = 3

// confirm вызывает ConfirmReady и требует отсутствия ошибки.
func confirm(t testing.TB, pool *pgxpool.Pool, head int64) int {
	t.Helper()
	n, err := payments.ConfirmReady(context.Background(), pool, head, confirmations)
	if err != nil {
		t.Fatalf("ConfirmReady(head=%d): %v", head, err)
	}
	return n
}

// assertLedger сверяет холд: на депозитах и в холде ровно amount, журнал сходится.
func assertLedger(t testing.TB, pool *pgxpool.Pool, amount money.Amount) {
	t.Helper()
	ctx := context.Background()
	deposits, err1 := ledger.Balance(ctx, pool, ledger.AccountDeposits)
	hold, err2 := ledger.Balance(ctx, pool, ledger.AccountHoldAML)
	if err1 != nil || err2 != nil {
		t.Fatalf("balances: %v, %v", err1, err2)
	}
	if deposits != amount || hold != -amount {
		t.Errorf("assets:deposits = %s, hold:aml = %s; want %s and %s", deposits, hold, amount, -amount)
	}
	if err := ledger.CheckBalanced(ctx, pool); err != nil {
		t.Errorf("CheckBalanced: %v", err)
	}
}

// Границы порога: блок B имеет N подтверждений при head = B + N - 1. До этого ни статуса, ни проводок.
func TestConfirmReadyThreshold(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 10))

	// Блок 10 при голове 10 имеет 1 подтверждение, при голове 11 — 2: мало.
	for _, head := range []int64{9, 10, 11} {
		if n := confirm(t, pool, head); n != 0 {
			t.Errorf("ConfirmReady(head=%d) = %d, want 0: %d confirmations are not enough", head, n, head-10+1)
		}
		if got := find(t, pool, "0xaa", 0); got.Status != payments.StatusDetected {
			t.Errorf("head %d: status = %s, want detected", head, got.Status)
		}
		assertLedger(t, pool, 0) // до порога проводок нет вообще
	}

	// Голова 12: у блока 10 ровно три подтверждения.
	if n := confirm(t, pool, 12); n != 1 {
		t.Fatalf("ConfirmReady(head=12) = %d, want 1", n)
	}
	if got := find(t, pool, "0xaa", 0); got.Status != payments.StatusAMLPending {
		t.Errorf("status = %s, want aml_pending", got.Status)
	}
	assertLedger(t, pool, amt(t, "100"))
}

// Порог в одно подтверждение: платёж готов, как только блок стал последним.
func TestConfirmReadySingleConfirmation(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 10))

	if n, err := payments.ConfirmReady(context.Background(), pool, 9, 1); err != nil || n != 0 {
		t.Errorf("ConfirmReady(head=9, N=1) = %d, %v; want 0", n, err)
	}
	if n, err := payments.ConfirmReady(context.Background(), pool, 10, 1); err != nil || n != 1 {
		t.Errorf("ConfirmReady(head=10, N=1) = %d, %v; want 1", n, err)
	}
}

func TestConfirmReadyRejectsBadConfirmations(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	for _, n := range []int{0, -1} {
		if _, err := payments.ConfirmReady(context.Background(), pool, 100, n); !errors.Is(err, payments.ErrInvalidConfirmations) {
			t.Errorf("ConfirmReady(N=%d) error = %v, want ErrInvalidConfirmations", n, err)
		}
	}
}

// Одна проводка hold с ключом {id}:1; повтор с тем же ключом и содержимым ничего не пишет.
func TestConfirmReadyPostsHoldOnce(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 10))
	id := find(t, pool, "0xaa", 0).ID

	if n := confirm(t, pool, 12); n != 1 {
		t.Fatalf("ConfirmReady = %d, want 1", n)
	}
	assertLedger(t, pool, amt(t, "100"))

	// Проводка под ключом {id}:1 уже есть: тот же Post не пишет ничего (posted = false).
	same := ledger.Posting{Key: key(id), Kind: "hold", Lines: []ledger.Line{
		ledger.Debit(ledger.AccountDeposits, amt(t, "100")),
		ledger.Credit(ledger.AccountHoldAML, amt(t, "100")),
	}}
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		posted, err := ledger.Post(ctx, tx, same)
		if posted {
			t.Error("the hold posting was written twice")
		}
		return err
	})
	if err != nil {
		t.Errorf("Post with the stored key: %v", err)
	}
}

// Повторный ConfirmReady ничего не делает: ни статуса, ни второй проводки.
func TestConfirmReadyRepeatDoesNothing(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 10))

	if n := confirm(t, pool, 12); n != 1 {
		t.Fatalf("first ConfirmReady = %d, want 1", n)
	}
	for _, head := range []int64{12, 12, 50} {
		if n := confirm(t, pool, head); n != 0 {
			t.Errorf("repeated ConfirmReady(head=%d) = %d, want 0", head, n)
		}
	}
	assertLedger(t, pool, amt(t, "100"))
}

// ConfirmReady берёт только detected: orphaned и платежи после порога не трогает.
func TestConfirmReadySkipsOtherStatuses(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	for i, status := range []payments.Status{
		payments.StatusOrphaned, payments.StatusAMLPending, payments.StatusCredited, payments.StatusRejected,
	} {
		tx := string(status)
		mustDetect(t, pool, params(t, tx, 0, int64(i+1)))
		setStatus(t, pool, find(t, pool, "0x"+tx, 0).ID, status)
	}
	if n := confirm(t, pool, 1000); n != 0 {
		t.Errorf("ConfirmReady = %d, want 0", n)
	}
	assertLedger(t, pool, 0)
}

// 20 параллельных ConfirmReady: каждый платёж переведён ровно одним вызовом и проведён один раз.
func TestConfirmReadyConcurrent(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const payers, workers = 5, 20
	for i := range payers {
		mustDetect(t, pool, params(t, string(rune('a'+i))+"0", 0, int64(10+i)))
	}

	results := make(chan int, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := payments.ConfirmReady(context.Background(), pool, 100, confirmations)
			results <- n
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("ConfirmReady: %v", err)
		}
	}
	total := 0
	for n := range results {
		total += n
	}
	if total != payers {
		t.Errorf("payments confirmed over all calls = %d, want %d: each one by exactly one call", total, payers)
	}
	assertLedger(t, pool, payers*amt(t, "100"))
}

// Ошибка проводки откатывает смену статуса этого платежа и не мешает остальным.
func TestConfirmReadyRollsBackStatusWithLedger(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 10))
	mustDetect(t, pool, params(t, "bb", 0, 10))
	bad := find(t, pool, "0xaa", 0).ID

	// Под ключом первого платежа уже лежит другая проводка: Post вернёт ErrKeyConflict.
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.Posting{Key: key(bad), Kind: "other", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountDeposits, amt(t, "1")),
			ledger.Credit(ledger.AccountRevenueFees, amt(t, "1")),
		}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := payments.ConfirmReady(ctx, pool, 100, confirmations)
	if !errors.Is(err, ledger.ErrKeyConflict) {
		t.Errorf("ConfirmReady error = %v, want ErrKeyConflict", err)
	}
	if n != 1 {
		t.Errorf("ConfirmReady = %d, want 1: the healthy payment is confirmed", n)
	}
	if got := find(t, pool, "0xaa", 0); got.Status != payments.StatusDetected {
		t.Errorf("failed payment status = %s, want detected: the status must roll back with the posting", got.Status)
	}
	if got := find(t, pool, "0xbb", 0); got.Status != payments.StatusAMLPending {
		t.Errorf("healthy payment status = %s, want aml_pending", got.Status)
	}
}

// key — ключ проводки шага 1.
func key(paymentID int64) string {
	return strconv.FormatInt(paymentID, 10) + ":1"
}
