package refunds_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/internal/refunds"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

const sender = "0x1111111111111111111111111111111111111111"

func amt(t testing.TB, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// params — возврат 100 USDT по платежу paymentID на адрес отправителя.
func params(t testing.TB, paymentID int64) refunds.CreateParams {
	t.Helper()
	return refunds.CreateParams{PaymentID: paymentID, ToAddress: sender, Amount: amt(t, "100")}
}

// create вызывает Create в собственной транзакции базы и фиксирует её.
func create(ctx context.Context, pool *pgxpool.Pool, p refunds.CreateParams) (r refunds.Refund, created bool, err error) {
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		r, created, err = refunds.Create(ctx, tx, p)
		return err
	})
	return r, created, err
}

func mustCreate(t testing.TB, pool *pgxpool.Pool, p refunds.CreateParams) (refunds.Refund, bool) {
	t.Helper()
	r, created, err := create(context.Background(), pool, p)
	if err != nil {
		t.Fatalf("Create(payment %d): %v", p.PaymentID, err)
	}
	return r, created
}

func mustMarkReady(t testing.TB, db refunds.Execer) int {
	t.Helper()
	n, err := refunds.MarkReady(context.Background(), db)
	if err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	return n
}

func countRefunds(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM refunds.refunds`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// storedStatus читает статус возврата прямо из таблицы.
func storedStatus(t testing.TB, pool *pgxpool.Pool, paymentID int64) refunds.Status {
	t.Helper()
	var s refunds.Status
	err := pool.QueryRow(context.Background(), `SELECT status FROM refunds.refunds WHERE payment_id = $1`, paymentID).Scan(&s)
	if err != nil {
		t.Fatalf("read status of the refund of payment %d: %v", paymentID, err)
	}
	return s
}

func TestCreate(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	r, created := mustCreate(t, pool, params(t, 7))
	if !created {
		t.Error("created = false for a new refund")
	}
	want := refunds.Refund{ID: r.ID, PaymentID: 7, ToAddress: sender, Amount: amt(t, "100"), Status: refunds.StatusCreated}
	if r != want || r.ID <= 0 {
		t.Errorf("Create = %+v, want %+v with a positive id", r, want)
	}
	if n := countRefunds(t, pool); n != 1 {
		t.Errorf("refunds in the table = %d, want 1", n)
	}
}

// Повтор по тому же платежу возвращает тот же возврат и не создаёт второй.
func TestCreateRepeatReturnsSameRefund(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	first, created := mustCreate(t, pool, params(t, 7))
	if !created {
		t.Fatal("first Create: created = false")
	}
	for range 3 {
		again, created := mustCreate(t, pool, params(t, 7))
		if created {
			t.Error("repeated Create: created = true, a second refund was made")
		}
		if again != first {
			t.Errorf("repeated Create = %+v, want the first refund %+v", again, first)
		}
	}
	if n := countRefunds(t, pool); n != 1 {
		t.Errorf("refunds in the table = %d, want 1", n)
	}

	// Возврат другого платежа — отдельная строка.
	other, created := mustCreate(t, pool, params(t, 8))
	if !created || other.ID == first.ID {
		t.Errorf("refund of another payment = %+v, created = %v; want a new refund", other, created)
	}
}

// Повтор не откатывает возврат из ready_to_send в created и не меняет сумму и адрес.
func TestCreateRepeatKeepsStoredRefund(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	first, _ := mustCreate(t, pool, params(t, 7))
	if n := mustMarkReady(t, pool); n != 1 {
		t.Fatalf("MarkReady = %d, want 1", n)
	}

	p := params(t, 7)
	p.Amount = amt(t, "1")
	p.ToAddress = "0x2222222222222222222222222222222222222222"
	again, created := mustCreate(t, pool, p)

	want := first
	want.Status = refunds.StatusReadyToSend
	if created || again != want {
		t.Errorf("repeated Create = %+v, created = %v; want the stored refund %+v and created = false", again, created, want)
	}
	if got := storedStatus(t, pool, 7); got != refunds.StatusReadyToSend {
		t.Errorf("status in the table = %s, want ready_to_send", got)
	}
}

// 20 параллельных Create по одному платежу: одна строка, один создатель, у всех тот же возврат.
func TestCreateConcurrent(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const workers = 20
	type result struct {
		r       refunds.Refund
		created bool
		err     error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, created, err := create(context.Background(), pool, params(t, 7))
			results <- result{r, created, err}
		}()
	}
	wg.Wait()
	close(results)

	creators := 0
	var ids = map[int64]bool{}
	for res := range results {
		if res.err != nil {
			t.Errorf("Create: %v", res.err)
			continue
		}
		if res.created {
			creators++
		}
		ids[res.r.ID] = true
	}
	if creators != 1 {
		t.Errorf("calls with created = true: %d, want exactly 1", creators)
	}
	if len(ids) != 1 {
		t.Errorf("distinct refund ids returned = %d, want 1", len(ids))
	}
	if n := countRefunds(t, pool); n != 1 {
		t.Errorf("refunds in the table = %d, want 1", n)
	}
}

// Возврат пишется в транзакцию вызывающего: откат убирает его, а после отката платёж
// снова можно вернуть.
func TestCreateRollsBackWithCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	errAbort := errors.New("abort")
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, created, err := refunds.Create(ctx, tx, params(t, 7)); err != nil || !created {
			t.Errorf("Create inside the transaction = created %v, %v; want true, nil", created, err)
		}
		return errAbort // как если бы дальше сорвалась проводка
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("InTx error = %v, want the abort error", err)
	}
	if n := countRefunds(t, pool); n != 0 {
		t.Fatalf("refunds after the rollback = %d, want 0", n)
	}

	if _, created := mustCreate(t, pool, params(t, 7)); !created {
		t.Error("Create after the rollback: created = false, want a new refund")
	}
}

// Параллельный Create ждёт чужую открытую транзакцию на уникальном индексе. Первая откатилась —
// возврат создаёт ожидавший: чужая невыполненная вставка возврат по платежу не занимает.
func TestCreateWaitsForOpenTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	firstInserted := make(chan struct{})
	release := make(chan struct{})
	errAbort := errors.New("abort")

	firstDone := make(chan struct{})
	go func() { // первая транзакция вставила возврат и держит его, пока её не отпустят
		defer close(firstDone)
		_ = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			if _, _, err := refunds.Create(ctx, tx, params(t, 7)); err != nil {
				t.Errorf("first Create: %v", err)
			}
			close(firstInserted)
			<-release
			return errAbort
		})
	}()
	<-firstInserted

	secondDone := make(chan struct{})
	var created bool
	go func() { // вторая встаёт в ожидание на уникальном индексе
		defer close(secondDone)
		var err error
		_, created, err = create(ctx, pool, params(t, 7))
		if err != nil {
			t.Errorf("second Create: %v", err)
		}
	}()

	waitUntilBlocked(t, pool, secondDone)
	if n := countRefunds(t, pool); n != 0 {
		t.Errorf("refunds visible while the first transaction is open = %d, want 0", n)
	}
	close(release) // первая откатывается
	<-firstDone
	<-secondDone

	if !created {
		t.Error("second Create: created = false after the first one rolled back, want a new refund")
	}
	if n := countRefunds(t, pool); n != 1 {
		t.Errorf("refunds in the table = %d, want 1", n)
	}
}

// waitUntilBlocked крутится, пока в базе не появится запрос, ждущий чужую транзакцию, или
// пока не закроется done (запрос уже закончился, не заблокировавшись). Таймеров нет: смотрим
// состояние базы, а тест ограничен общим таймаутом go test.
func waitUntilBlocked(t testing.TB, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	for {
		select {
		case <-done:
			return
		default:
		}
		var waiting int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		runtime.Gosched()
	}
}

// Адрес приводится к нижнему регистру (в базе CHECK).
func TestCreateLowercasesAddress(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	p := params(t, 7)
	p.ToAddress = "0xABCDEFabcdef1111111111111111111111111111"
	r, _ := mustCreate(t, pool, p)

	want := "0xabcdefabcdef1111111111111111111111111111"
	if r.ToAddress != want {
		t.Errorf("Create returned address %q, want %q", r.ToAddress, want)
	}
	var stored string
	if err := pool.QueryRow(context.Background(), `SELECT to_address FROM refunds.refunds`).Scan(&stored); err != nil || stored != want {
		t.Errorf("address in the table = %q, %v; want %q", stored, err, want)
	}
}

func TestCreateRejectsInvalidParams(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	tests := []struct {
		name   string
		mutate func(*refunds.CreateParams)
		want   error
	}{
		{"zero payment id", func(p *refunds.CreateParams) { p.PaymentID = 0 }, refunds.ErrInvalidParams},
		{"negative payment id", func(p *refunds.CreateParams) { p.PaymentID = -1 }, refunds.ErrInvalidParams},
		{"empty address", func(p *refunds.CreateParams) { p.ToAddress = "" }, refunds.ErrInvalidParams},
		{"zero amount", func(p *refunds.CreateParams) { p.Amount = 0 }, refunds.ErrNonPositiveAmount},
		{"negative amount", func(p *refunds.CreateParams) { p.Amount = -1 }, refunds.ErrNonPositiveAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params(t, 7)
			tt.mutate(&p)
			if _, _, err := create(context.Background(), pool, p); !errors.Is(err, tt.want) {
				t.Errorf("Create error = %v, want %v", err, tt.want)
			}
		})
	}
	if n := countRefunds(t, pool); n != 0 {
		t.Errorf("refunds in the table = %d after rejected calls, want 0", n)
	}
}

// MarkReady переводит created в ready_to_send; повтор ничего не меняет.
func TestMarkReadyTwice(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	if n := mustMarkReady(t, pool); n != 0 {
		t.Errorf("MarkReady without refunds = %d, want 0", n)
	}
	mustCreate(t, pool, params(t, 7))
	mustCreate(t, pool, params(t, 8))

	if n := mustMarkReady(t, pool); n != 2 {
		t.Errorf("first MarkReady = %d, want 2", n)
	}
	for range 2 {
		if n := mustMarkReady(t, pool); n != 0 {
			t.Errorf("repeated MarkReady = %d, want 0", n)
		}
	}
	for _, id := range []int64{7, 8} {
		if got := storedStatus(t, pool, id); got != refunds.StatusReadyToSend {
			t.Errorf("status of the refund of payment %d = %s, want ready_to_send", id, got)
		}
	}

	// Новый возврат после MarkReady ждёт следующего вызова, готовые не затрагиваются.
	mustCreate(t, pool, params(t, 9))
	if n := mustMarkReady(t, pool); n != 1 {
		t.Errorf("MarkReady after a new refund = %d, want 1", n)
	}
}

// Параллельные MarkReady: каждый возврат переведён ровно одним вызовом.
func TestMarkReadyConcurrent(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const refundsCount, workers = 5, 20
	for i := range refundsCount {
		mustCreate(t, pool, params(t, int64(i+1)))
	}

	moved := make(chan int, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := refunds.MarkReady(context.Background(), pool)
			if err != nil {
				t.Errorf("MarkReady: %v", err)
			}
			moved <- n
		}()
	}
	wg.Wait()
	close(moved)

	total := 0
	for n := range moved {
		total += n
	}
	if total != refundsCount {
		t.Errorf("refunds moved over all calls = %d, want %d: each one by exactly one call", total, refundsCount)
	}
}

func TestByPayments(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)
	first, _ := mustCreate(t, pool, params(t, 7))
	second, _ := mustCreate(t, pool, params(t, 8))
	mustMarkReady(t, pool)

	got, err := refunds.ByPayments(ctx, pool, []int64{7, 8, 999})
	if err != nil {
		t.Fatal(err)
	}
	first.Status, second.Status = refunds.StatusReadyToSend, refunds.StatusReadyToSend
	if len(got) != 2 || got[7] != first || got[8] != second {
		t.Errorf("ByPayments = %+v, want refunds of payments 7 and 8 only: %+v, %+v", got, first, second)
	}

	for _, ids := range [][]int64{nil, {}, {999}} {
		got, err := refunds.ByPayments(ctx, pool, ids)
		if err != nil || len(got) != 0 {
			t.Errorf("ByPayments(%v) = %+v, %v; want an empty map", ids, got, err)
		}
	}
}
