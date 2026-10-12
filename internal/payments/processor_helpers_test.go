package payments_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Помощники тестов обработчика платежей. Время в тестах не ждётся: обработчику подставляются
// часы fakeClock, и тест сам решает, когда пауза между попытками «истекла».

const (
	// dirty — адрес из чёрного списка AML-мока; sender (helpers_test.go) — чистый.
	dirty = "0x000000000000000000000000000000000000dead"
	// partnerID — партнёр по умолчанию у платежей из params.
	partnerID = 7
)

// fakeClock — часы теста. Безопасны для нескольких обработчиков в разных горутинах.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newClock ставит время на час вперёд от настоящего, с точностью до миллисекунды. Запас нужен,
// чтобы платёж, созданный по часам базы, был готов к проверке и при небольшом расхождении часов
// базы и теста; миллисекунды — чтобы время без потерь проходило через timestamptz (микросекунды).
func newClock() *fakeClock {
	return &fakeClock{now: time.Now().Add(time.Hour).Truncate(time.Millisecond)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// checkerFunc превращает функцию в aml.Checker: тесту нужны проверки с особым поведением.
type checkerFunc func(ctx context.Context, address string) (aml.Verdict, error)

func (f checkerFunc) Check(ctx context.Context, address string) (aml.Verdict, error) {
	return f(ctx, address)
}

// countingChecker считает вызовы настоящего мока.
type countingChecker struct {
	*aml.Mock
	mu    sync.Mutex
	calls int
}

func newCountingChecker() *countingChecker {
	return &countingChecker{Mock: aml.NewMock([]string{dirty})}
}

func (c *countingChecker) Check(ctx context.Context, address string) (aml.Verdict, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Mock.Check(ctx, address)
}

func (c *countingChecker) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// syncBuffer — буфер лога, безопасный для записи из нескольких горутин.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env — база с подставными часами и AML-моком. Партнёр partnerID уже заведён в ledger.
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	clock *fakeClock
	aml   *countingChecker
	logs  *syncBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, pool: pgtest.NewMigrated(t), clock: newClock(), aml: newCountingChecker(), logs: &syncBuffer{}}
	e.ensurePartner(partnerID)
	return e
}

// ensurePartner заводит счёт partner:{id} в ledger, как это делает создание партнёра.
func (e *env) ensurePartner(id int64) {
	e.t.Helper()
	ctx := context.Background()
	err := postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		return ledger.EnsureAccount(ctx, tx, ledger.PartnerAccount(id))
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// processor возвращает обработчик с проверкой checker и часами теста. Все обработчики одного
// теста работают с одной базой и одними часами: так выглядят два экземпляра сервиса.
func (e *env) processor(checker aml.Checker) *payments.Processor {
	p := payments.NewProcessor(e.pool, checker, slog.New(slog.NewTextHandler(e.logs, nil)))
	p.SetClock(e.clock.Now)
	return p
}

// tick делает проход обработчика с обычным моком и требует отсутствия ошибки.
func (e *env) tick() {
	e.t.Helper()
	if err := e.processor(e.aml).Tick(context.Background()); err != nil {
		e.t.Fatalf("Tick: %v", err)
	}
}

// seedOption меняет параметры тестового платежа.
type seedOption func(*payments.DetectParams)

func from(address string) seedOption {
	return func(p *payments.DetectParams) { p.From = address }
}

func amount(t testing.TB, s string) seedOption {
	a := amt(t, s)
	return func(p *payments.DetectParams) { p.Amount = a }
}

func feeBps(bps int) seedOption {
	return func(p *payments.DetectParams) { p.FeeBps = bps }
}

func partner(id int64) seedOption {
	return func(p *payments.DetectParams) { p.PartnerID = id }
}

// hashOf — имя транзакции для платежа номер i; seed допишет к нему 0x.
func hashOf(i int) string { return "e" + strconv.Itoa(i) }

// seed создаёт платёж в aml_pending: обнаружен, порог пройден, шаг 1 проведён (деньги в холде).
// По умолчанию — 100 USDT от чистого адреса партнёру partnerID со ставкой 1,5%. Возвращает id.
func (e *env) seed(tx string, opts ...seedOption) int64 {
	e.t.Helper()
	p := params(e.t, tx, 0, 10)
	p.PartnerID = partnerID
	for _, opt := range opts {
		opt(&p)
	}
	mustDetect(e.t, e.pool, p)
	confirm(e.t, e.pool, p.BlockNumber+confirmations-1)
	return find(e.t, e.pool, p.TxHash, 0).ID
}

// paymentState — платёж в виде, удобном для проверок решения.
type paymentState struct {
	Status        payments.Status
	Verdict       *string
	CheckedAt     *time.Time
	Attempts      int
	NextAttemptAt time.Time
}

func (e *env) state(id int64) paymentState {
	e.t.Helper()
	var s paymentState
	err := e.pool.QueryRow(context.Background(), `
		SELECT status, aml_verdict, aml_checked_at, aml_attempts, next_attempt_at
		FROM payments.payments WHERE id = $1`, id).
		Scan(&s.Status, &s.Verdict, &s.CheckedAt, &s.Attempts, &s.NextAttemptAt)
	if err != nil {
		e.t.Fatalf("read payment %d: %v", id, err)
	}
	return s
}

// assertDecided проверяет итог решения: статус, вердикт (он пишется вместе со статусом) и время проверки.
func (e *env) assertDecided(id int64, status payments.Status, verdict aml.Verdict) {
	e.t.Helper()
	s := e.state(id)
	if s.Status != status || s.Verdict == nil || *s.Verdict != string(verdict) {
		e.t.Errorf("payment %d: status = %s, verdict = %v; want %s and %s", id, s.Status, s.Verdict, status, verdict)
	}
	if s.CheckedAt == nil || !s.CheckedAt.Equal(e.clock.Now()) {
		e.t.Errorf("payment %d: aml_checked_at = %v, want the clock time %v", id, s.CheckedAt, e.clock.Now())
	}
}

// assertPending проверяет, что платёж ждёт решения: статус aml_pending, вердикта нет.
func (e *env) assertPending(id int64) {
	e.t.Helper()
	if s := e.state(id); s.Status != payments.StatusAMLPending || s.Verdict != nil || s.CheckedAt != nil {
		e.t.Errorf("payment %d: status = %s, verdict = %v, checked_at = %v; want aml_pending without a verdict",
			id, s.Status, s.Verdict, s.CheckedAt)
	}
}

// assertBooks сверяет сальдо перечисленных счетов (дебет > 0, кредит < 0) и баланс журнала.
func (e *env) assertBooks(want map[string]money.Amount) {
	e.t.Helper()
	ctx := context.Background()
	got, err := ledger.Totals(ctx, e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	for code, w := range want {
		if g := got[code]; g != w {
			e.t.Errorf("ledger %s = %s, want %s", code, g, w)
		}
	}
	if err := ledger.CheckBalanced(ctx, e.pool); err != nil {
		e.t.Errorf("CheckBalanced: %v", err)
	}
}

// postingKey возвращает ключ проводки шага step платежа id.
func postingKey(id int64, step int) string {
	return strconv.FormatInt(id, 10) + ":" + strconv.Itoa(step)
}

// postingKind возвращает вид проводки под ключом; found = false, если проводки нет.
func (e *env) postingKind(key string) (kind string, found bool) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(),
		`SELECT kind FROM ledger.transactions WHERE idempotency_key = $1`, key).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return kind, true
}

// journalSize возвращает число проводок и число строк журнала в целом.
func (e *env) journalSize() (postings, entries int) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM ledger.transactions), (SELECT count(*) FROM ledger.entries)`).Scan(&postings, &entries)
	if err != nil {
		e.t.Fatal(err)
	}
	return postings, entries
}

// entryCount возвращает число строк проводки под ключом.
func (e *env) entryCount(key string) int {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM ledger.entries en JOIN ledger.transactions t ON t.id = en.transaction_id
		WHERE t.idempotency_key = $1`, key).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// refundStatuses возвращает статусы всех возвратов в порядке создания.
func (e *env) refundStatuses() []string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT status FROM refunds.refunds ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	statuses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.t.Fatal(err)
	}
	return statuses
}

// waitUntilBlocked крутится, пока в базе не появится запрос, ждущий чужую блокировку строки, или
// пока не закроется done. Возвращает true, если запрос заблокирован. Таймеров нет: смотрим
// состояние базы, а зависший тест остановит общий таймаут go test.
func waitUntilBlocked(t testing.TB, pool *pgxpool.Pool, done <-chan struct{}) (blocked bool) {
	t.Helper()
	for {
		select {
		case <-done:
			return false
		default:
		}
		var waiting int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return true
		}
	}
}
