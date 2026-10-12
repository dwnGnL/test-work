package payments_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/refunds"
)

// Интеграционные тесты решения по платежу. Время подставляется часами fakeClock, ожиданий нет.

// Низкий риск, пример из дизайна: 100 USDT при 150 б.п. — партнёру 98.5, сервису 1.5.
func TestProcessorCreditsLowRisk(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa")

	e.tick()

	e.assertDecided(id, payments.StatusCredited, aml.VerdictLow)
	e.assertBooks(map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "100"),
		ledger.AccountHoldAML:        0,
		ledger.PartnerAccount(7):     -amt(t, "98.5"),
		ledger.AccountRevenueFees:    -amt(t, "1.5"),
		ledger.AccountRefundsPayable: 0,
	})
	if balance, err := ledger.PartnerBalance(context.Background(), e.pool, partnerID); err != nil || balance != amt(t, "98.5") {
		t.Errorf("PartnerBalance = %s, %v; want 98.500000", balance, err)
	}
	if kind, found := e.postingKind(postingKey(id, 2)); !found || kind != "credit" {
		t.Errorf("step 2 posting = %q (found %v), want credit", kind, found)
	}
	if got := e.refundStatuses(); len(got) != 0 {
		t.Errorf("refunds = %v, want none for a credited payment", got)
	}
}

// Нулевая комиссия не попадает в проводку строкой; стопроцентная — тоже не даёт нулевой строки партнёру.
func TestProcessorZeroAndFullFee(t *testing.T) {
	for _, tt := range []struct {
		name        string
		bps         int
		wantPartner string
		wantFees    string
		wantEntries int
	}{
		{"zero fee", 0, "100", "0", 2},
		{"full fee", 10_000, "0", "100", 2},
		{"ordinary fee", 150, "98.5", "1.5", 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed("aa", feeBps(tt.bps))

			e.tick()

			e.assertDecided(id, payments.StatusCredited, aml.VerdictLow)
			e.assertBooks(map[string]money.Amount{
				ledger.AccountHoldAML:     0,
				ledger.PartnerAccount(7):  -amt(t, tt.wantPartner),
				ledger.AccountRevenueFees: -amt(t, tt.wantFees),
			})
			if n := e.entryCount(postingKey(id, 2)); n != tt.wantEntries {
				t.Errorf("step 2 posting has %d entries, want %d: a zero line must not be written", n, tt.wantEntries)
			}
		})
	}
}

// Высокий риск: платёж rejected, один возврат на полную сумму на адрес отправителя, партнёру ничего.
func TestProcessorRejectsHighRisk(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa", from(dirty))

	e.tick()

	e.assertDecided(id, payments.StatusRejected, aml.VerdictHigh)
	e.assertBooks(map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "100"),
		ledger.AccountHoldAML:        0,
		ledger.AccountRefundsPayable: -amt(t, "100"),
		ledger.PartnerAccount(7):     0,
		ledger.AccountRevenueFees:    0,
	})
	if kind, found := e.postingKind(postingKey(id, 2)); !found || kind != "refund" {
		t.Errorf("step 2 posting = %q (found %v), want refund", kind, found)
	}

	got, err := refunds.ByPayments(context.Background(), e.pool, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	want := refunds.Refund{PaymentID: id, ToAddress: dirty, Amount: amt(t, "100"), Status: refunds.StatusReadyToSend}
	r := got[id]
	want.ID = r.ID
	if len(got) != 1 || r != want {
		t.Errorf("refunds = %+v, want one refund %+v: full amount to the sender, ready_to_send after Tick", got, want)
	}
}

// Повтор не создаёт ничего нового: ни второго возврата, ни второй проводки, ни обращения к AML,
// ни сдвига балансов. Новый обработчик на той же базе — то же самое (рестарт сервиса).
func TestProcessorRepeatChangesNothing(t *testing.T) {
	e := newEnv(t)
	clean := e.seed("aa")
	bad := e.seed("bb", from(dirty))
	e.tick()
	e.assertDecided(clean, payments.StatusCredited, aml.VerdictLow)
	e.assertDecided(bad, payments.StatusRejected, aml.VerdictHigh)
	postings, entries := e.journalSize()
	calls := e.aml.Calls()

	for range 3 {
		e.clock.Advance(payments.AMLMaxPause)
		e.tick()
	}
	e.clock.Advance(payments.AMLMaxPause)
	if err := e.processor(e.aml).Tick(context.Background()); err != nil { // «после рестарта»
		t.Fatal(err)
	}

	if p, en := e.journalSize(); p != postings || en != entries {
		t.Errorf("journal = %d postings, %d entries after repeats; want %d, %d", p, en, postings, entries)
	}
	if got := e.refundStatuses(); !slices.Equal(got, []string{"ready_to_send"}) {
		t.Errorf("refunds = %v, want exactly one ready_to_send", got)
	}
	if got := e.aml.Calls(); got != calls {
		t.Errorf("AML was asked %d times, want %d: decided payments are not checked again", got, calls)
	}
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML:        0,
		ledger.PartnerAccount(7):     -amt(t, "98.5"),
		ledger.AccountRevenueFees:    -amt(t, "1.5"),
		ledger.AccountRefundsPayable: -amt(t, "100"),
	})
}

// AML недоступен: деньги остаются в холде, партнёру ничего, платёж ждёт и проверяется снова,
// когда истекла пауза. Часы стоят — повторный Tick платёж не берёт.
func TestProcessorAMLUnavailable(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa")
	t0 := e.clock.Now()
	e.aml.SetAvailable(false)

	e.tick()

	e.assertPending(id)
	s := e.state(id)
	if s.Attempts != 1 || !s.NextAttemptAt.Equal(t0.Add(payments.AMLFirstPause)) {
		t.Errorf("attempts = %d, next attempt at +%v; want 1 and +%v", s.Attempts, s.NextAttemptAt.Sub(t0), payments.AMLFirstPause)
	}
	// В журнале только hold; партнёру и в доход ничего не попало.
	if postings, _ := e.journalSize(); postings != 1 {
		t.Errorf("journal has %d postings, want 1 (the hold only)", postings)
	}
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML:    -amt(t, "100"),
		ledger.PartnerAccount(7): 0, ledger.AccountRevenueFees: 0, ledger.AccountRefundsPayable: 0,
	})
	if _, found := e.postingKind(postingKey(id, 2)); found {
		t.Error("step 2 posting exists while AML is unavailable")
	}
	if got := e.logs.String(); !containsAll(got, "AML check failed", "payment_id=") {
		t.Errorf("log = %q, want the failed check to be logged", got)
	}

	// Часы стоят: платёж на паузе, обработчик его не берёт и AML не спрашивает.
	e.aml.SetAvailable(true)
	calls := e.aml.Calls()
	e.tick()
	e.clock.Advance(payments.AMLFirstPause - time.Millisecond)
	e.tick()
	if got := e.aml.Calls(); got != calls {
		t.Errorf("AML was asked %d more times before the pause was over, want 0", got-calls)
	}
	e.assertPending(id)
	if got := e.state(id).Attempts; got != 1 {
		t.Errorf("attempts = %d before the pause was over, want 1", got)
	}

	// Пауза истекла, AML снова работает: платёж зачислен.
	e.clock.Advance(time.Millisecond)
	e.tick()
	e.assertDecided(id, payments.StatusCredited, aml.VerdictLow)
	if got := e.state(id).Attempts; got != 2 {
		t.Errorf("attempts = %d after the second check, want 2", got)
	}
	e.assertBooks(map[string]money.Amount{ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): -amt(t, "98.5")})
}

// Любая ошибка проверки, не только ErrUnavailable, оставляет платёж на проверке: вердикта нет —
// решения нет. Вердикт, которого код не знает, тоже не зачисляет и не отклоняет.
func TestProcessorNoDecisionWithoutVerdict(t *testing.T) {
	for _, tt := range []struct {
		name    string
		checker checkerFunc
		wantErr error
	}{
		{"unavailable", func(context.Context, string) (aml.Verdict, error) { return "", aml.ErrUnavailable }, nil},
		{"other error", func(context.Context, string) (aml.Verdict, error) { return "", errors.New("provider timeout") }, nil},
		{"error together with a verdict", func(context.Context, string) (aml.Verdict, error) {
			return aml.VerdictLow, errors.New("half an answer")
		}, nil},
		{"unknown verdict", func(context.Context, string) (aml.Verdict, error) { return "medium", nil }, payments.ErrUnknownVerdict},
		{"empty verdict", func(context.Context, string) (aml.Verdict, error) { return "", nil }, payments.ErrUnknownVerdict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed("aa")

			err := e.processor(tt.checker).Tick(context.Background())

			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("Tick error = %v, want %v", err, tt.wantErr)
			}
			e.assertPending(id)
			e.assertBooks(map[string]money.Amount{ledger.AccountHoldAML: -amt(t, "100"), ledger.PartnerAccount(7): 0})
			if got := e.refundStatuses(); len(got) != 0 {
				t.Errorf("refunds = %v, want none", got)
			}
		})
	}
}

// Пауза между попытками растёт вдвое и упирается в потолок. Огромное число попыток (долгая
// недоступность AML) не ломает захват.
func TestProcessorPauseGrowsAndIsCapped(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa")
	e.aml.SetAvailable(false)

	want := payments.AMLFirstPause
	for attempt := 1; attempt <= 10; attempt++ {
		e.tick()
		s := e.state(id)
		if got := s.NextAttemptAt.Sub(e.clock.Now()); s.Attempts != attempt || got != want {
			t.Fatalf("attempt %d: attempts = %d, pause = %v; want %d and %v", attempt, s.Attempts, got, attempt, want)
		}
		e.clock.Advance(want)
		want = min(2*want, payments.AMLMaxPause)
	}
	if want != payments.AMLMaxPause {
		t.Fatalf("the test never reached the cap: pause %v", want)
	}

	_, err := e.pool.Exec(context.Background(), `UPDATE payments.payments SET aml_attempts = 100000 WHERE id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	e.tick()
	if got := e.state(id).NextAttemptAt.Sub(e.clock.Now()); got != payments.AMLMaxPause {
		t.Errorf("pause after 100000 attempts = %v, want the cap %v", got, payments.AMLMaxPause)
	}
}

// «Рестарт»: обработчик остановился после захвата, до решения. Платёж не потерян: когда пауза
// истекла, новый обработчик доводит его до конца, и проводка шага 2 ровно одна.
func TestProcessorRecoversAfterCrash(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa")
	t0 := e.clock.Now()

	// Первый обработчик «падает» на проверке: контекст отменяется, решения нет.
	ctx, cancel := context.WithCancel(context.Background())
	crashing := checkerFunc(func(ctx context.Context, _ string) (aml.Verdict, error) {
		cancel()
		return "", ctx.Err()
	})
	if err := e.processor(crashing).Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick of the crashed worker = %v, want context.Canceled", err)
	}
	e.assertPending(id)
	if s := e.state(id); s.Attempts != 1 || !s.NextAttemptAt.Equal(t0.Add(payments.AMLFirstPause)) {
		t.Errorf("after the crash: attempts = %d, next attempt at +%v; want 1 and +%v",
			s.Attempts, s.NextAttemptAt.Sub(t0), payments.AMLFirstPause)
	}

	// Платёж захвачен: новый обработчик до конца паузы его не трогает.
	e.tick()
	e.assertPending(id)
	if e.aml.Calls() != 0 {
		t.Fatal("a new worker took a payment that is still leased by the crashed one")
	}

	e.clock.Advance(payments.AMLFirstPause)
	e.tick()
	e.assertDecided(id, payments.StatusCredited, aml.VerdictLow)
	if postings, _ := e.journalSize(); postings != 2 {
		t.Errorf("journal has %d postings, want 2 (hold and one step 2)", postings)
	}
	e.assertBooks(map[string]money.Amount{ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): -amt(t, "98.5")})
}

// Ошибка по одному платежу не останавливает остальные и откатывает смену его статуса.
func TestProcessorErrorDoesNotStopOthers(t *testing.T) {
	e := newEnv(t)
	broken := e.seed("aa", partner(8)) // у партнёра 8 нет счёта в ledger: проводка не пройдёт
	healthy := e.seed("bb")

	err := e.processor(e.aml).Tick(context.Background())

	if !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("Tick error = %v, want ErrUnknownAccount", err)
	}
	e.assertPending(broken) // статус откатился вместе с проводкой
	e.assertDecided(healthy, payments.StatusCredited, aml.VerdictLow)
	if _, found := e.postingKind(postingKey(broken, 2)); found {
		t.Error("the broken payment has a step 2 posting")
	}

	// Счёт появился — платёж проходит после паузы.
	e.ensurePartner(8)
	e.clock.Advance(payments.AMLFirstPause)
	e.tick()
	e.assertDecided(broken, payments.StatusCredited, aml.VerdictLow)
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): -amt(t, "98.5"), ledger.PartnerAccount(8): -amt(t, "98.5"),
	})
}

// Если статус платежа каким-то образом откатился в aml_pending, а проводка шага 2 уже есть,
// второй исход не проходит: ключ проводки один на оба исхода и держит сам, без смены статуса.
func TestProcessorOneKeyForBothOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name         string
		first, again string // адрес отправителя определяет вердикт мока
		wantStored   string // вид проводки, которая должна остаться
	}{
		{"credit then refund", sender, dirty, "credit"},
		{"refund then credit", dirty, sender, "refund"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.seed("aa", from(tt.first))
			e.tick()
			postings, entries := e.journalSize()

			// Возвращаем платёж в очередь в обход условной смены статуса. Для CHECK вердикта
			// достаточно сбросить его вместе со статусом.
			_, err := e.pool.Exec(context.Background(), `
				UPDATE payments.payments
				SET status = 'aml_pending', aml_verdict = NULL, aml_checked_at = NULL, from_address = $2,
				    next_attempt_at = $3
				WHERE id = $1`, id, tt.again, e.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(payments.AMLFirstPause)

			err = e.processor(e.aml).Tick(context.Background())

			var conflict *ledger.KeyConflictError
			if !errors.Is(err, ledger.ErrKeyConflict) || !errors.As(err, &conflict) || conflict.StoredKind != tt.wantStored {
				t.Fatalf("Tick error = %v, want ErrKeyConflict with the stored kind %q", err, tt.wantStored)
			}
			if p, en := e.journalSize(); p != postings || en != entries {
				t.Errorf("journal = %d postings, %d entries; want %d, %d: the second outcome must not be posted", p, en, postings, entries)
			}
			e.assertPending(id) // смена статуса откатилась вместе с ошибкой
			if kind, _ := e.postingKind(postingKey(id, 2)); kind != tt.wantStored {
				t.Errorf("step 2 posting kind = %q, want %q", kind, tt.wantStored)
			}
			if tt.wantStored == "credit" && len(e.refundStatuses()) != 0 {
				t.Errorf("refunds = %v, want none: the payment was credited", e.refundStatuses())
			}
		})
	}
}

// Возвраты, оставшиеся в created (проход упал после коммита решения, до MarkReady), довозит
// следующий проход, даже если платежей в очереди нет.
func TestProcessorMovesLeftoverRefunds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	err := postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		_, _, err := refunds.Create(ctx, tx, refunds.CreateParams{PaymentID: 1, ToAddress: dirty, Amount: amt(t, "5")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.refundStatuses(); !slices.Equal(got, []string{"created"}) {
		t.Fatalf("refunds = %v, want one created", got)
	}

	e.tick()

	if got := e.refundStatuses(); !slices.Equal(got, []string{"ready_to_send"}) {
		t.Errorf("refunds after Tick = %v, want one ready_to_send", got)
	}
}

// Пачка ограничена: за проход обработчик решает не больше AMLBatchSize платежей.
func TestProcessorBatchLimit(t *testing.T) {
	e := newEnv(t)
	total := payments.AMLBatchSize + 5
	ids := make([]int64, total)
	for i := range ids {
		ids[i] = e.seed(hashOf(i))
	}

	e.tick()
	if decided := e.countStatus(payments.StatusCredited); decided != payments.AMLBatchSize {
		t.Fatalf("credited after the first Tick = %d, want %d", decided, payments.AMLBatchSize)
	}
	e.tick()
	if decided := e.countStatus(payments.StatusCredited); decided != total {
		t.Errorf("credited after the second Tick = %d, want %d", decided, total)
	}
	e.assertBooks(map[string]money.Amount{ledger.AccountHoldAML: 0})
}

func (e *env) countStatus(status payments.Status) int {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payments.payments WHERE status = $1`, string(status)).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Два платежа на один счёт (60 и 40 из 100): после первого счёт partially_paid, после второго
// paid. Статус счёта считается по сумме платежей до комиссии (payments.Summary).
func TestProcessorInvoiceStatus(t *testing.T) {
	e := newEnv(t)
	inv := invoices.Invoice{Amount: amt(t, "100"), ExpiresAt: e.clock.Now().Add(time.Hour)}
	status := func() string {
		t.Helper()
		received, inProgress, err := payments.Summary(context.Background(), e.pool, invoiceA)
		if err != nil {
			t.Fatal(err)
		}
		return invoices.Status(inv, received, inProgress, e.clock.Now())
	}

	e.seed("aa", amount(t, "60"))
	if got := status(); got != invoices.StatusPending {
		t.Errorf("invoice status before the AML answer = %s, want pending", got)
	}
	e.tick()
	if got := status(); got != invoices.StatusPartiallyPaid {
		t.Errorf("invoice status after 60 of 100 = %s, want partially_paid", got)
	}

	e.seed("bb", amount(t, "40"))
	e.tick()
	if got := status(); got != invoices.StatusPaid {
		t.Errorf("invoice status after 60 and 40 = %s, want paid", got)
	}
	// 100 USDT при 1,5%: партнёру 98.5, сервису 1.5 — по двум платежам 59.1 + 39.4 и 0.9 + 0.6.
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): -amt(t, "98.5"), ledger.AccountRevenueFees: -amt(t, "1.5"),
	})
}

// Переплата (120 на счёт 100) зачисляется целиком, счёт paid.
func TestProcessorOverpayment(t *testing.T) {
	e := newEnv(t)
	inv := invoices.Invoice{Amount: amt(t, "100"), ExpiresAt: e.clock.Now().Add(time.Hour)}
	e.seed("aa", amount(t, "120"))

	e.tick()

	received, inProgress, err := payments.Summary(context.Background(), e.pool, invoiceA)
	if err != nil {
		t.Fatal(err)
	}
	if received != amt(t, "120") || inProgress {
		t.Errorf("Summary = %s, in progress %v; want 120.000000 and nothing in progress", received, inProgress)
	}
	if got := invoices.Status(inv, received, inProgress, e.clock.Now()); got != invoices.StatusPaid {
		t.Errorf("invoice status = %s, want paid", got)
	}
	// Комиссия с 120 при 1,5% — 1.8, партнёру 118.2: переплата зачислена вся.
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML: 0, ledger.PartnerAccount(7): -amt(t, "118.2"), ledger.AccountRevenueFees: -amt(t, "1.8"),
	})
}

// База сама не пускает платёж в credited без вердикта low и в rejected без high (миграция 0010).
// Обработчик ставит статус и вердикт одним оператором, поэтому защита его не задевает.
func TestDatabaseRefusesDecisionWithoutVerdict(t *testing.T) {
	e := newEnv(t)
	id := e.seed("aa")

	for _, tt := range []struct {
		name       string
		set        string
		constraint string
	}{
		{"credited without a verdict", `status = 'credited'`, "credited_only_low"},
		{"credited with a high verdict", `status = 'credited', aml_verdict = 'high'`, "credited_only_low"},
		{"rejected without a verdict", `status = 'rejected'`, "rejected_only_high"},
		{"rejected with a low verdict", `status = 'rejected', aml_verdict = 'low'`, "rejected_only_high"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.pool.Exec(context.Background(), `UPDATE payments.payments SET `+tt.set+` WHERE id = $1`, id)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != tt.constraint {
				t.Errorf("update error = %v, want check violation of %s", err, tt.constraint)
			}
			e.assertPending(id)
		})
	}
}

// containsAll сообщает, что s содержит все подстроки.
func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
