package payments_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Параллельная обработка: 20 горутин вызывают Tick на одной базе. Каждый платёж решён один раз,
// спрошен у AML один раз, проводка шага 2 одна.
func TestProcessorConcurrentTicks(t *testing.T) {
	e := newEnv(t)
	pgtest.WarmUp(t, e.pool)

	const clean, bad, workers = 5, 3, 20
	var ids []int64
	for i := range clean {
		ids = append(ids, e.seed(hashOf(i)))
	}
	for i := range bad {
		ids = append(ids, e.seed(hashOf(100+i), from(dirty)))
	}

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.processor(e.aml).Tick(context.Background())
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("Tick: %v", err)
		}
	}
	if got := e.aml.Calls(); got != clean+bad {
		t.Errorf("AML was asked %d times, want %d: each payment is claimed by one worker", got, clean+bad)
	}
	for i, id := range ids {
		if i < clean {
			e.assertDecided(id, payments.StatusCredited, aml.VerdictLow)
		} else {
			e.assertDecided(id, payments.StatusRejected, aml.VerdictHigh)
		}
		if n := e.entryCount(postingKey(id, 2)); n == 0 {
			t.Errorf("payment %d has no step 2 posting", id)
		}
	}
	if postings, _ := e.journalSize(); postings != 2*len(ids) {
		t.Errorf("journal has %d postings, want %d: a hold and one step 2 for each payment", postings, 2*len(ids))
	}
	if got := e.refundStatuses(); len(got) != bad {
		t.Errorf("refunds = %v, want %d", got, bad)
	}
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML:        0,
		ledger.PartnerAccount(7):     -clean * amt(t, "98.5"),
		ledger.AccountRevenueFees:    -clean * amt(t, "1.5"),
		ledger.AccountRefundsPayable: -bad * amt(t, "100"),
	})
}

// Гонка разных ответов AML (решение 3 журнала). Первый обработчик взял платёж и завис в AML, его
// пауза истекла, платёж взял второй, и они получили РАЗНЫЕ вердикты. Решает тот, кто успел
// первым; опоздавший выходит без ошибки и без проводок. Итог один: одна проводка шага 2,
// возврат только при rejected, сумма не ушла и партнёру, и отправителю.
func TestProcessorDifferentVerdictsRace(t *testing.T) {
	for _, tt := range []struct {
		name        string
		slowVerdict aml.Verdict // ответ зависшего обработчика, он приходит последним
		fastVerdict aml.Verdict // ответ второго обработчика, он решает первым
		wantStatus  payments.Status
		wantKind    string
		wantRefunds int
	}{
		{"low is late, high decides", aml.VerdictLow, aml.VerdictHigh, payments.StatusRejected, "refund", 1},
		{"high is late, low decides", aml.VerdictHigh, aml.VerdictLow, payments.StatusCredited, "credit", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed("aa")

			entered := make(chan struct{})
			release := make(chan struct{})
			slow := checkerFunc(func(context.Context, string) (aml.Verdict, error) {
				close(entered) // первый обработчик уже захватил платёж и ждёт ответа AML
				<-release
				return tt.slowVerdict, nil
			})
			fast := checkerFunc(func(context.Context, string) (aml.Verdict, error) { return tt.fastVerdict, nil })

			slowDone := make(chan error, 1)
			go func() { slowDone <- e.processor(slow).Tick(context.Background()) }()
			select {
			case <-entered:
			case err := <-slowDone: // проход кончился, не дойдя до AML: иначе тест ждал бы вечно
				t.Fatalf("the first Tick finished without asking AML: %v", err)
			}

			// Пауза истекла: платёж взял второй обработчик и решил его.
			e.clock.Advance(payments.AMLFirstPause)
			if err := e.processor(fast).Tick(context.Background()); err != nil {
				t.Errorf("fast Tick: %v", err)
			}
			close(release) // и только теперь первый получил свой, противоположный ответ
			if err := <-slowDone; err != nil {
				t.Errorf("late Tick: %v, want no error: the payment was already decided", err)
			}

			e.assertDecided(id, tt.wantStatus, tt.fastVerdict)
			if kind, found := e.postingKind(postingKey(id, 2)); !found || kind != tt.wantKind {
				t.Errorf("step 2 posting = %q (found %v), want %q", kind, found, tt.wantKind)
			}
			if postings, _ := e.journalSize(); postings != 2 {
				t.Errorf("journal has %d postings, want 2 (the hold and one step 2)", postings)
			}
			if got := e.refundStatuses(); len(got) != tt.wantRefunds {
				t.Errorf("refunds = %v, want %d", got, tt.wantRefunds)
			}
			want := map[string]money.Amount{ledger.AccountHoldAML: 0}
			if tt.wantStatus == payments.StatusCredited {
				want[ledger.PartnerAccount(7)], want[ledger.AccountRefundsPayable] = -amt(t, "98.5"), 0
			} else {
				want[ledger.PartnerAccount(7)], want[ledger.AccountRefundsPayable] = 0, -amt(t, "100")
			}
			e.assertBooks(want)
		})
	}
}

// AML вызывается вне транзакции базы: в момент Check у обработчика нет открытых транзакций,
// а значит, нет ни соединения, занятого ожиданием ответа, ни блокировок на платежах.
func TestProcessorCallsAMLOutsideTransaction(t *testing.T) {
	e := newEnv(t)
	e.seed("aa")
	e.seed("bb", from(dirty))

	var calls, openTransactions atomic.Int32
	spy := checkerFunc(func(ctx context.Context, address string) (aml.Verdict, error) {
		var open int
		err := e.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&open)
		if err != nil {
			t.Errorf("look at pg_stat_activity: %v", err)
		}
		calls.Add(1)
		openTransactions.Add(int32(open))
		return e.aml.Check(ctx, address)
	})

	if err := e.processor(spy).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("AML was asked %d times, want 2", calls.Load())
	}
	if got := openTransactions.Load(); got != 0 {
		t.Errorf("open transactions seen during AML calls = %d, want 0: AML must be called outside a transaction", got)
	}
}

// Захват не ждёт чужих блокировок: платёж, занятый чужой транзакцией, пропускается (SKIP LOCKED),
// остальные берутся сразу. Освободившийся платёж берёт следующий проход.
func TestProcessorClaimSkipsLockedPayments(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	locked := e.seed("aa")
	free := e.seed("bb")

	// Чужая транзакция держит строку первого платежа.
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if err := holder.Rollback(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	defer release()
	if _, err := holder.Exec(ctx, `SELECT id FROM payments.payments WHERE id = $1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}

	tickDone := make(chan struct{})
	var tickErr error
	go func() {
		defer close(tickDone)
		tickErr = e.processor(e.aml).Tick(ctx)
	}()
	if waitUntilBlocked(t, e.pool, tickDone) {
		t.Error("Tick is waiting for a locked payment, want it to skip the payment (FOR UPDATE SKIP LOCKED)")
		release() // иначе Tick не завершится
	}
	<-tickDone
	if tickErr != nil {
		t.Fatalf("Tick: %v", tickErr)
	}

	e.assertDecided(free, payments.StatusCredited, aml.VerdictLow)
	if s := e.state(locked); s.Status != payments.StatusAMLPending || s.Attempts != 0 {
		t.Errorf("locked payment: status = %s, attempts = %d; want aml_pending and 0: it was not claimed", s.Status, s.Attempts)
	}

	release()
	e.tick()
	e.assertDecided(locked, payments.StatusCredited, aml.VerdictLow)
}

// То же, но ответы приходят одновременно: оба обработчика взяли платёж, оба получили разные
// вердикты и одновременно идут в транзакцию решения. Кто выиграет — неизвестно, а итог один:
// проигравший ждёт победителя на строке платежа, видит, что статус сменён, и выходит без ошибки.
func TestProcessorSimultaneousDifferentVerdicts(t *testing.T) {
	for _, tt := range []struct {
		name        string
		first, last aml.Verdict
	}{
		{"low and high", aml.VerdictLow, aml.VerdictHigh},
		{"high and low", aml.VerdictHigh, aml.VerdictLow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed("aa")

			release := make(chan struct{})
			var entered [2]chan struct{}
			var done [2]chan error
			for i, verdict := range []aml.Verdict{tt.first, tt.last} {
				entered[i], done[i] = make(chan struct{}), make(chan error, 1)
				checker := checkerFunc(func(context.Context, string) (aml.Verdict, error) {
					close(entered[i])
					<-release
					return verdict, nil
				})
				go func() { done[i] <- e.processor(checker).Tick(context.Background()) }()
				select {
				case <-entered[i]:
				case err := <-done[i]: // проход кончился, не дойдя до AML: иначе тест ждал бы вечно
					t.Fatalf("Tick %d finished without asking AML: %v", i, err)
				}
				e.clock.Advance(payments.AMLFirstPause) // пауза истекла, платёж берёт следующий
			}
			close(release) // оба ответа приходят одновременно

			for i := range done {
				if err := <-done[i]; err != nil {
					t.Errorf("Tick %d: %v, want no error: the loser exits quietly", i, err)
				}
			}

			s := e.state(id)
			if s.Status != payments.StatusCredited && s.Status != payments.StatusRejected {
				t.Fatalf("status = %s, want credited or rejected", s.Status)
			}
			wantKind, wantRefunds := "credit", 0
			partner, payable := -amt(t, "98.5"), money.Amount(0)
			if s.Status == payments.StatusRejected {
				wantKind, wantRefunds = "refund", 1
				partner, payable = 0, -amt(t, "100")
			}
			if kind, found := e.postingKind(postingKey(id, 2)); !found || kind != wantKind {
				t.Errorf("status %s, step 2 posting = %q (found %v), want %q", s.Status, kind, found, wantKind)
			}
			if postings, _ := e.journalSize(); postings != 2 {
				t.Errorf("journal has %d postings, want 2 (the hold and one step 2)", postings)
			}
			if got := e.refundStatuses(); len(got) != wantRefunds {
				t.Errorf("status %s, refunds = %v, want %d", s.Status, got, wantRefunds)
			}
			e.assertBooks(map[string]money.Amount{
				ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): partner, ledger.AccountRefundsPayable: payable,
			})
		})
	}
}
