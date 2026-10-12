package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/httpapi"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/partners"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/internal/scanner"
)

// Сквозные тесты ходят в Postgres: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

const (
	// Настройки сервиса, как в docker-compose.yml.
	confirmations = 3
	invoiceTTL    = time.Hour
	usdtContract  = "0xdAC17F958D2ee523a2206206994597C13D831ec7"

	// Партнёры: у alpha комиссия 1,5%, у beta 0,5%.
	alphaKey    = "e2e-key-alpha"
	betaKey     = "e2e-key-beta"
	alphaFeeBps = 150
	betaFeeBps  = 50

	// Адреса «внешнего мира». dirtySender стоит в чёрном списке AML-мока, cleanSender нет.
	cleanSender    = "0x1111111111111111111111111111111111111111"
	dirtySender    = "0x000000000000000000000000000000000000dead"
	unknownAddress = "0x9999999999999999999999999999999999999999" // на него не выставлен ни один счёт
	fakeToken      = "0x00000000000000000000000000000000000000f4" // «USDT» с чужим адресом контракта

	zero = "0.000000" // нулевая сумма в ответах API

	// Виды проводок ledger: шаг 1 (холд) и два исхода шага 2.
	kindHold   = "hold"
	kindCredit = "credit"
	kindRefund = "refund"
)

// env — окружение теста: база, HTTP-слой, AML-мок и «процесс сервиса» со сканером и обработчиком.
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool // пул базы теста: через него работают HTTP-слой и проверки
	mock   *aml.Mock     // «внешний провайдер AML»: переживает рестарт сервиса
	server *httptest.Server
	logs   *syncBuffer
	log    *slog.Logger
	alpha  *partnerClient
	beta   *partnerClient

	// «Процесс сервиса»: свой пул соединений, сканер и обработчик платежей. restart заменяет
	// их новыми, база остаётся прежней.
	svcPool   *pgxpool.Pool
	scanner   *scanner.Scanner
	processor *payments.Processor
}

// partnerClient — HTTP-клиент партнёра: все его запросы идут с его API-ключом.
type partnerClient struct {
	e   *env
	id  int64 // для сверки баланса с ledger
	key string
}

// response — ответ сервиса: код и тело.
type response struct {
	status int
	body   []byte
}

// invoice — счёт в ответе API. Суммы строками, как в JSON.
type invoice struct {
	ID             string    `json:"id"`
	Amount         string    `json:"amount"`
	AmountReceived string    `json:"amount_received"`
	Status         string    `json:"status"`
	DepositAddress string    `json:"deposit_address"`
	Payments       []payment `json:"payments"`
}

// payment — платёж в ответе по счёту.
type payment struct {
	TxHash      string  `json:"tx_hash"`
	Amount      string  `json:"amount"`
	Status      string  `json:"status"`
	BlockNumber *int64  `json:"block_number"`
	Refund      *refund `json:"refund"`
}

// refund — возврат отклонённого платежа.
type refund struct {
	Status    string `json:"status"`
	ToAddress string `json:"to_address"`
	Amount    string `json:"amount"`
}

// books — системные счета ledger. В журнале дебет > 0, кредит < 0; здесь холд, комиссия и
// возвраты показаны положительными числами, как баланс партнёра: сколько сервис удерживает,
// заработал и должен вернуть.
type books struct {
	Deposits money.Amount // assets:deposits — USDT, лежащие на депозитных адресах
	Held     money.Amount // hold:aml — платежи ждут решения AML
	Fees     money.Amount // revenue:fees — комиссия сервиса
	Refunds  money.Amount // refunds:payable — долг перед отправителями отклонённых платежей
}

// headBody — ответ ручек /sim/blocks и /sim/reorg.
type headBody struct {
	Head struct {
		Number int64 `json:"number"`
	} `json:"head"`
}

// ticker — то, что нужно параллельному тесту от сканера и обработчика платежей.
type ticker interface {
	Tick(ctx context.Context) error
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

// newEnv собирает сервис так же, как cmd/server/main.go, на временной базе. Лог сервиса
// печатается, только если тест упал.
func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.NewMigrated(t)
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("service log:\n%s", logs)
		}
	})

	e := &env{
		t: t, pool: pool, mock: aml.NewMock([]string{dirtySender}),
		logs: logs, log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	e.alpha = e.addPartner("alpha", alphaKey, alphaFeeBps)
	e.beta = e.addPartner("beta", betaKey, betaFeeBps)

	e.server = httptest.NewServer(httpapi.New(pool, e.log, httpapi.Options{
		InvoiceTTL: invoiceTTL, Simulator: chain.NewSimulator(pool), AML: e.mock, USDTContract: usdtContract,
	}))
	t.Cleanup(e.server.Close)

	e.boot()
	return e
}

// addPartner создаёт партнёра с комиссией bps базисных пунктов, как это делает код сервиса.
func (e *env) addPartner(name, key string, bps int) *partnerClient {
	e.t.Helper()
	ctx := context.Background()
	var p partners.Partner
	err := postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		p, err = partners.Create(ctx, tx, name, key, bps)
		return err
	})
	if err != nil {
		e.t.Fatalf("create partner %s: %v", name, err)
	}
	return &partnerClient{e: e, id: p.ID, key: key}
}

// boot запускает «процесс сервиса»: свой пул соединений к той же базе, сканер и обработчик.
func (e *env) boot() {
	e.t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), e.pool.Config())
	if err != nil {
		e.t.Fatalf("open the service pool: %v", err)
	}
	e.t.Cleanup(pool.Close)
	e.svcPool = pool
	e.scanner = e.newScanner()
	e.processor = e.newProcessor()
}

// restart имитирует перезапуск сервиса: пул, сканер и обработчик выбрасываются и создаются
// заново. Курсор сканера, очередь платежей и проводки остаются в базе.
func (e *env) restart() {
	e.t.Helper()
	e.svcPool.Close()
	e.boot()
}

// newScanner возвращает ещё один экземпляр сканера на пуле сервиса.
func (e *env) newScanner() *scanner.Scanner {
	return scanner.New(e.svcPool, chain.NewSimulator(e.svcPool), confirmations, usdtContract, e.log)
}

// newProcessor возвращает ещё один экземпляр обработчика платежей на пуле сервиса.
func (e *env) newProcessor() *payments.Processor {
	return payments.NewProcessor(e.svcPool, e.mock, e.log)
}

// scan делает проход сканера: читает новые блоки, находит платежи, подтверждает набравшие порог.
func (e *env) scan() {
	e.t.Helper()
	if err := e.scanner.Tick(context.Background()); err != nil {
		e.t.Fatalf("scanner Tick: %v", err)
	}
}

// process делает проход обработчика платежей: AML-проверка и зачисление или возврат.
func (e *env) process() {
	e.t.Helper()
	if err := e.processor.Tick(context.Background()); err != nil {
		e.t.Fatalf("payment processor Tick: %v", err)
	}
}

// makeDue заменяет ожидание. После неудачной проверки AML платёж ждёт паузу в несколько секунд
// и больше, а тест ничего не ждёт: он объявляет паузу истёкшей, и следующий проход обработчика
// берёт платёж сразу. Саму паузу (рост, потолок, захват одним воркером) подробно проверяют
// тесты пакета payments на подменённых часах.
func (e *env) makeDue() {
	e.t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`UPDATE payments.payments SET next_attempt_at = to_timestamp(0) WHERE status = 'aml_pending'`)
	if err != nil {
		e.t.Fatalf("make payments due: %v", err)
	}
}

// do отправляет HTTP-запрос сервису. apiKey пустой — без входа; headers — пары «имя, значение».
func (e *env) do(method, path, apiKey, body string, headers ...string) response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return response{status: resp.StatusCode, body: data}
}

// transfer отправляет в сеть перевод USDT с адреса from на адрес to и возвращает хэш транзакции.
// Перевод лежит в мемпуле, пока не добыт блок.
func (e *env) transfer(from, to, amount string) string {
	e.t.Helper()
	return e.transferToken("", from, to, amount)
}

// transferToken то же, но токен задан явно; пустой — USDT по умолчанию.
func (e *env) transferToken(token, from, to, amount string) string {
	e.t.Helper()
	body := fmt.Sprintf(`{"from":%q,"to":%q,"amount":%q`, from, to, amount)
	if token != "" {
		body += fmt.Sprintf(`,"token":%q`, token)
	}
	r := e.do(http.MethodPost, "/sim/transfers", "", body+"}")
	wantStatus(e.t, r, http.StatusCreated)
	return decode[struct {
		TxHash string `json:"tx_hash"`
	}](e.t, r).TxHash
}

// mine добывает count блоков: первый забирает мемпул, остальные пустые — подтверждения.
// Возвращает номер головы цепочки.
func (e *env) mine(count int) int64 {
	e.t.Helper()
	r := e.do(http.MethodPost, "/sim/blocks", "", fmt.Sprintf(`{"count":%d}`, count))
	wantStatus(e.t, r, http.StatusCreated)
	return decode[headBody](e.t, r).Head.Number
}

// reorg выбрасывает последние depth блоков. dropTransfers: переводы выброшенных блоков исчезают,
// иначе возвращаются в мемпул. Возвращает номер головы после реорга.
func (e *env) reorg(depth int, dropTransfers bool) int64 {
	e.t.Helper()
	r := e.do(http.MethodPost, "/sim/reorg", "", fmt.Sprintf(`{"depth":%d,"drop_transfers":%t}`, depth, dropTransfers))
	wantStatus(e.t, r, http.StatusOK)
	return decode[headBody](e.t, r).Head.Number
}

// setAML включает и выключает AML-мок ручкой симулятора.
func (e *env) setAML(available bool) {
	e.t.Helper()
	wantStatus(e.t, e.do(http.MethodPost, "/sim/aml", "", fmt.Sprintf(`{"available":%t}`, available)), http.StatusOK)
}

// usdt разбирает сумму вида "98.5".
func (e *env) usdt(s string) money.Amount {
	e.t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		e.t.Fatalf("amount %q: %v", s, err)
	}
	return a
}

// books читает сальдо системных счетов ledger.
func (e *env) books() books {
	e.t.Helper()
	totals, err := ledger.Totals(context.Background(), e.pool)
	if err != nil {
		e.t.Fatalf("ledger totals: %v", err)
	}
	return books{
		Deposits: totals[ledger.AccountDeposits],
		Held:     -totals[ledger.AccountHoldAML],
		Fees:     -totals[ledger.AccountRevenueFees],
		Refunds:  -totals[ledger.AccountRefundsPayable],
	}
}

// wantBooks сверяет системные счета ledger и проверяет, что журнал в целом сходится в ноль.
func (e *env) wantBooks(want books) {
	e.t.Helper()
	if got := e.books(); got != want {
		e.t.Fatalf("ledger accounts = %+v, want %+v", got, want)
	}
	if err := ledger.CheckBalanced(context.Background(), e.pool); err != nil {
		e.t.Fatalf("ledger is not balanced: %v", err)
	}
}

// wantPostings сверяет, сколько проводок каждого вида записано в журнал. nil — журнал пуст.
func (e *env) wantPostings(want map[string]int) {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT kind, count(*)::int FROM ledger.transactions GROUP BY kind`)
	if err != nil {
		e.t.Fatalf("count postings: %v", err)
	}
	got := make(map[string]int)
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			e.t.Fatalf("count postings: %v", err)
		}
		got[kind] = n
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("count postings: %v", err)
	}
	if !maps.Equal(got, want) {
		e.t.Fatalf("postings by kind = %v, want %v", got, want)
	}
}

// count возвращает число строк, которое даёт запрос с одним числовым столбцом.
func (e *env) count(query string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// createInvoiceRaw отправляет запрос на создание счёта на amount USDT с ключом идемпотентности key.
func (p *partnerClient) createInvoiceRaw(key, amount string) response {
	p.e.t.Helper()
	return p.e.do(http.MethodPost, "/v1/invoices", p.key, fmt.Sprintf(`{"amount":%q}`, amount), "Idempotency-Key", key)
}

// createInvoice создаёт счёт и требует 201: счёт новый.
func (p *partnerClient) createInvoice(key, amount string) invoice {
	p.e.t.Helper()
	r := p.createInvoiceRaw(key, amount)
	wantStatus(p.e.t, r, http.StatusCreated)
	return decode[invoice](p.e.t, r)
}

// getInvoiceRaw запрашивает счёт по id.
func (p *partnerClient) getInvoiceRaw(id string) response {
	p.e.t.Helper()
	return p.e.do(http.MethodGet, "/v1/invoices/"+id, p.key, "")
}

// wantInvoice читает счёт через API и сверяет статус счёта, полученную сумму (до комиссии) и
// статусы платежей по порядку создания. Возвращает счёт для дальнейших проверок.
func (p *partnerClient) wantInvoice(id, status, received string, paymentStatuses ...string) invoice {
	p.e.t.Helper()
	r := p.getInvoiceRaw(id)
	wantStatus(p.e.t, r, http.StatusOK)
	inv := decode[invoice](p.e.t, r)
	got := make([]string, len(inv.Payments))
	for i, pay := range inv.Payments {
		got[i] = pay.Status
	}
	if inv.Status != status || inv.AmountReceived != received || !slices.Equal(got, paymentStatuses) {
		p.e.t.Fatalf("invoice %s: status %q, amount_received %q, payments %v; want %q, %q, %v",
			id, inv.Status, inv.AmountReceived, got, status, received, paymentStatuses)
	}
	return inv
}

// wantBalance сверяет баланс партнёра: ответ API и сальдо его счёта в ledger должны совпасть с want.
func (p *partnerClient) wantBalance(want string) {
	p.e.t.Helper()
	r := p.e.do(http.MethodGet, "/v1/balance", p.key, "")
	wantStatus(p.e.t, r, http.StatusOK)
	viaAPI := decode[struct {
		Balance string `json:"balance"`
	}](p.e.t, r).Balance

	inLedger, err := ledger.PartnerBalance(context.Background(), p.e.pool, p.id)
	if err != nil {
		p.e.t.Fatalf("ledger balance of partner %d: %v", p.id, err)
	}
	if viaAPI != want || inLedger.String() != want {
		p.e.t.Fatalf("balance of partner %d: API %q, ledger %q; want %q", p.id, viaAPI, inLedger, want)
	}
}

// onlyPayment возвращает единственный платёж счёта; другое число платежей — ошибка теста.
func onlyPayment(t testing.TB, inv invoice) payment {
	t.Helper()
	if len(inv.Payments) != 1 {
		t.Fatalf("invoice %s has %d payments, want exactly 1: %+v", inv.ID, len(inv.Payments), inv.Payments)
	}
	return inv.Payments[0]
}

// wantStatus требует код ответа status.
func wantStatus(t testing.TB, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d; body: %s", r.status, status, r.body)
	}
}

// wantError требует ответ-ошибку с кодом status и кодом ошибки code.
func wantError(t testing.TB, r response, status int, code string) {
	t.Helper()
	wantStatus(t, r, status)
	if got := decode[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, r).Error.Code; got != code {
		t.Fatalf("error code = %q, want %q; body: %s", got, code, r.body)
	}
}

// decode разбирает тело ответа в T.
func decode[T any](t testing.TB, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("response is not the expected JSON: %v\nstatus %d, body: %s", err, r.status, r.body)
	}
	return v
}
