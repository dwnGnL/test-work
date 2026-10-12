package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Тесты платежей в ответе счёта и ручек симулятора. Время не ждём: сеть двигают ручки /sim/*,
// сканер вызывается напрямую (tick).

const sender = "0x1111111111111111111111111111111111111111"

var txHashRe = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

type paymentRow struct {
	TxHash      string `json:"tx_hash"`
	Amount      string `json:"amount"`
	Status      string `json:"status"`
	BlockNumber *int64 `json:"block_number"`
}

// invoiceWithPayments — ответ по счёту вместе с платежами (invoiceRow без массива остаётся
// сравнимым значением для старых тестов).
type invoiceWithPayments struct {
	invoiceRow
	Payments []paymentRow `json:"payments"`
}

type headRow struct {
	Head struct {
		Number     int64  `json:"number"`
		Hash       string `json:"hash"`
		ParentHash string `json:"parent_hash"`
	} `json:"head"`
}

func (f *fixture) tick() {
	f.t.Helper()
	if err := f.sc.Tick(context.Background()); err != nil {
		f.t.Fatalf("scanner Tick: %v", err)
	}
}

// invoice читает счёт через API.
func (f *fixture) invoice(apiKey, id string) invoiceWithPayments {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/invoices/"+id, apiKey, "")
	wantStatus(f.t, rec, http.StatusOK)
	return decode[invoiceWithPayments](f.t, rec)
}

// transfer отправляет перевод на адрес to ручкой симулятора и возвращает хэш транзакции.
func (f *fixture) transfer(to, amount string) string {
	f.t.Helper()
	rec := f.do(http.MethodPost, "/sim/transfers", "", fmt.Sprintf(`{"to":%q,"from":%q,"amount":%q}`, to, sender, amount))
	wantStatus(f.t, rec, http.StatusCreated)
	hash := decode[struct {
		TxHash string `json:"tx_hash"`
	}](f.t, rec).TxHash
	if !txHashRe.MatchString(hash) {
		f.t.Fatalf("tx_hash = %q, want 0x and 64 lowercase hex digits", hash)
	}
	return hash
}

// mine добывает count блоков ручкой симулятора и возвращает номер головы.
func (f *fixture) mine(count int) int64 {
	f.t.Helper()
	rec := f.do(http.MethodPost, "/sim/blocks", "", fmt.Sprintf(`{"count":%d}`, count))
	wantStatus(f.t, rec, http.StatusCreated)
	return decode[headRow](f.t, rec).Head.Number
}

// creditAll зачисляет платежи счёта напрямую в базе. Решение AML появится на следующем этапе;
// здесь важно только то, что ответ по счёту считает полученное по платежам credited.
func (f *fixture) creditAll(invoiceID string) {
	f.t.Helper()
	_, err := f.pool.Exec(context.Background(), `
		UPDATE payments.payments SET status = 'credited', aml_verdict = 'low' WHERE invoice_id = $1::uuid`, invoiceID)
	if err != nil {
		f.t.Fatal(err)
	}
}

// Счёт показывает платёж: detected с первого блока, aml_pending после трёх подтверждений.
// Полученное остаётся нулевым, пока нет зачислений.
func TestInvoiceShowsPaymentLifecycle(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	// Платежей нет: массив пустой, а не null.
	rec := f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"payments":[]`) {
		t.Errorf("body = %s, want an empty payments array", rec.Body)
	}

	// Перевод ушёл в мемпул, блока нет: сканеру видеть нечего.
	txHash := f.transfer(inv.DepositAddress, "60")
	f.tick()
	if got := f.invoice(keyAlpha, inv.ID); len(got.Payments) != 0 {
		t.Fatalf("payments = %+v before the transfer is mined, want none", got.Payments)
	}

	// Блок 1: платёж detected, в обработке, получено ноль.
	if head := f.mine(1); head != 1 {
		t.Fatalf("head = %d, want 1", head)
	}
	f.tick()
	got := f.invoice(keyAlpha, inv.ID)
	block1 := int64(1)
	want := paymentRow{TxHash: txHash, Amount: "60.000000", Status: "detected", BlockNumber: &block1}
	if len(got.Payments) != 1 || got.Payments[0].TxHash != want.TxHash || got.Payments[0].Amount != want.Amount ||
		got.Payments[0].Status != want.Status || got.Payments[0].BlockNumber == nil || *got.Payments[0].BlockNumber != 1 {
		t.Fatalf("payments = %+v, want %+v", got.Payments, want)
	}
	if got.AmountReceived != "0.000000" || got.Status != "pending" {
		t.Errorf("amount_received = %q, status = %q; want 0.000000 and pending", got.AmountReceived, got.Status)
	}

	// Два подтверждения мало.
	f.mine(1)
	f.tick()
	if got := f.invoice(keyAlpha, inv.ID); got.Payments[0].Status != "detected" {
		t.Errorf("status with 2 confirmations = %q, want detected", got.Payments[0].Status)
	}

	// Три подтверждения: aml_pending, но не зачислено.
	f.mine(1)
	f.tick()
	got = f.invoice(keyAlpha, inv.ID)
	if got.Payments[0].Status != "aml_pending" || got.Payments[0].BlockNumber == nil || *got.Payments[0].BlockNumber != 1 {
		t.Errorf("payment = %+v, want aml_pending in block 1", got.Payments[0])
	}
	if got.AmountReceived != "0.000000" || got.Status != "pending" {
		t.Errorf("amount_received = %q, status = %q; want 0.000000 and pending until the credit", got.AmountReceived, got.Status)
	}
	if rec := f.do(http.MethodGet, "/v1/balance", keyAlpha, ""); rec.Body.String() != `{"balance":"0.000000"}` {
		t.Errorf("balance = %s, want 0.000000: nothing is credited before the AML answer", rec.Body)
	}

	// Платёж виден только владельцу счёта.
	wantError(t, f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyBeta, ""), http.StatusNotFound, "not_found")

	// Срок вышел, но платёж ещё в обработке: счёт не expired, исход не решён.
	f.api.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if got := f.invoice(keyAlpha, inv.ID); got.Status != "pending" {
		t.Errorf("status of an expired invoice with a payment in progress = %q, want pending", got.Status)
	}
}

// Полученное и статус считаются по зачисленным платежам, сумма — до комиссии.
func TestInvoiceAmountReceivedCountsCredited(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody)) // счёт на 100

	f.transfer(inv.DepositAddress, "60")
	f.mine(3)
	f.tick()
	f.creditAll(inv.ID)
	got := f.invoice(keyAlpha, inv.ID)
	if got.AmountReceived != "60.000000" || got.Status != "partially_paid" {
		t.Errorf("amount_received = %q, status = %q; want 60.000000 and partially_paid", got.AmountReceived, got.Status)
	}

	f.transfer(inv.DepositAddress, "40")
	f.mine(3)
	f.tick()
	// Второй платёж ещё не зачислен: он в обработке и в полученное не входит.
	got = f.invoice(keyAlpha, inv.ID)
	if got.AmountReceived != "60.000000" || got.Status != "partially_paid" || len(got.Payments) != 2 {
		t.Errorf("amount_received = %q, status = %q, payments = %d; want 60.000000, partially_paid, 2",
			got.AmountReceived, got.Status, len(got.Payments))
	}

	f.creditAll(inv.ID)
	got = f.invoice(keyAlpha, inv.ID)
	if got.AmountReceived != "100.000000" || got.Status != "paid" {
		t.Errorf("amount_received = %q, status = %q; want 100.000000 and paid (fee is not subtracted)", got.AmountReceived, got.Status)
	}
}

// Платёж из выброшенного блока виден как orphaned без блока и счёт не держит в обработке.
func TestInvoiceShowsOrphanedPayment(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	f.transfer(inv.DepositAddress, "60")
	f.mine(1)
	f.tick()

	rec := f.do(http.MethodPost, "/sim/reorg", "", `{"depth":1,"drop_transfers":true}`)
	wantStatus(t, rec, http.StatusOK)
	if head := decode[headRow](t, rec).Head; head.Number != 0 || head.Hash != "" {
		t.Errorf("head after reorging the whole chain = %+v, want an empty one", head)
	}
	f.tick()

	rec = f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, "")
	got := decode[invoiceWithPayments](t, rec)
	if len(got.Payments) != 1 || got.Payments[0].Status != "orphaned" || got.Payments[0].BlockNumber != nil {
		t.Fatalf("payments = %+v, want one orphaned payment without a block", got.Payments)
	}
	if !strings.Contains(rec.Body.String(), `"block_number":null`) {
		t.Errorf("body = %s, want block_number null", rec.Body)
	}
	if got.AmountReceived != "0.000000" || got.Status != "pending" {
		t.Errorf("amount_received = %q, status = %q", got.AmountReceived, got.Status)
	}

	// Просроченный счёт с одним откатившимся платежом — expired: в обработке ничего нет.
	f.api.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if got := f.invoice(keyAlpha, inv.ID); got.Status != "expired" {
		t.Errorf("status of an expired invoice with an orphaned payment = %q, want expired", got.Status)
	}
}

// Токен в запросе не обязателен (по умолчанию USDT из конфигурации); перевод другого токена
// сканер не принимает, а USDT принимает в любой записи адреса.
func TestSimTransferToken(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	send := func(extra string) {
		t.Helper()
		body := fmt.Sprintf(`{"to":%q,"from":%q,"amount":"1"%s}`, inv.DepositAddress, sender, extra)
		wantStatus(t, f.do(http.MethodPost, "/sim/transfers", "", body), http.StatusCreated)
	}
	send(``)                                                                       // токен по умолчанию
	send(fmt.Sprintf(`,"token":%q`, "0xdAC17F958D2ee523a2206206994597C13D831ec7")) // USDT с контрольной суммой
	send(fmt.Sprintf(`,"token":%q`, "0x00000000000000000000000000000000000000f4")) // поддельный токен
	f.mine(1)
	f.tick()

	if got := f.invoice(keyAlpha, inv.ID); len(got.Payments) != 2 {
		t.Errorf("payments = %+v, want 2: the fake token is ignored", got.Payments)
	}
}

// Ручки симулятора при выключенном флаге недоступны: маршрутов нет, ответ — обычный 404.
func TestSimDisabled(t *testing.T) {
	f := newFixture(t)
	off := &API{db: f.pool, log: f.api.log, invoiceTTL: testTTL, now: time.Now} // sim == nil
	handler := off.routes()

	for _, tt := range []struct{ method, path, body string }{
		{http.MethodPost, "/sim/transfers", `{"to":"0x1","from":"0x2","amount":"1"}`},
		{http.MethodPost, "/sim/blocks", `{"count":1}`},
		{http.MethodPost, "/sim/reorg", `{"depth":1}`},
		{http.MethodGet, "/sim/blocks", ""},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
			wantError(t, rec, http.StatusNotFound, "not_found")
		})
	}

	// Остальное API работает как раньше.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	wantStatus(t, rec, http.StatusOK)
}

func TestSimMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/sim/transfers", "/sim/blocks", "/sim/reorg"} {
		rec := f.do(http.MethodGet, path, "", "")
		wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := rec.Header().Get("Allow"); got != "POST" {
			t.Errorf("%s: Allow = %q, want POST", path, got)
		}
	}
}

// Неверный ввод — 400 в общем формате; состояние сети не меняется.
func TestSimBadInput(t *testing.T) {
	f := newFixture(t)
	to := "0x2222222222222222222222222222222222222222"
	transfer := func(format string, args ...any) string { return fmt.Sprintf(format, args...) }

	tests := []struct {
		name string
		path string
		body string
		code string
	}{
		{"transfer: empty body", "/sim/transfers", ``, "invalid_body"},
		{"transfer: not json", "/sim/transfers", `to=1`, "invalid_body"},
		{"transfer: array", "/sim/transfers", `[]`, "invalid_body"},
		{"transfer: unknown field", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":"1","gas":1}`, to, sender), "invalid_body"},
		{"transfer: to is a number", "/sim/transfers", `{"to":1,"from":"x","amount":"1"}`, "invalid_body"},
		{"transfer: amount is a number", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":5}`, to, sender), "invalid_body"},
		{"transfer: amount is null", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":null}`, to, sender), "invalid_amount"},
		{"transfer: no amount", "/sim/transfers", transfer(`{"to":%q,"from":%q}`, to, sender), "invalid_amount"},
		{"transfer: amount letters", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":"abc"}`, to, sender), "invalid_amount"},
		{"transfer: negative amount", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":"-1"}`, to, sender), "invalid_amount"},
		{"transfer: seven decimals", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":"1.0000001"}`, to, sender), "invalid_amount"},
		{"transfer: no recipient", "/sim/transfers", transfer(`{"from":%q,"amount":"1"}`, sender), "invalid_transfer"},
		{"transfer: bad recipient", "/sim/transfers", transfer(`{"to":"nope","from":%q,"amount":"1"}`, sender), "invalid_transfer"},
		{"transfer: bad sender", "/sim/transfers", transfer(`{"to":%q,"from":"0x12","amount":"1"}`, to), "invalid_transfer"},
		{"transfer: bad token", "/sim/transfers", transfer(`{"to":%q,"from":%q,"amount":"1","token":"usdt"}`, to, sender), "invalid_transfer"},

		{"blocks: empty body", "/sim/blocks", ``, "invalid_body"},
		{"blocks: count as a string", "/sim/blocks", `{"count":"3"}`, "invalid_body"},
		{"blocks: fractional count", "/sim/blocks", `{"count":1.5}`, "invalid_body"},
		{"blocks: unknown field", "/sim/blocks", `{"count":1,"n":2}`, "invalid_body"},
		{"blocks: no count", "/sim/blocks", `{}`, "invalid_count"},
		{"blocks: zero", "/sim/blocks", `{"count":0}`, "invalid_count"},
		{"blocks: negative", "/sim/blocks", `{"count":-1}`, "invalid_count"},
		{"blocks: too many", "/sim/blocks", `{"count":1001}`, "invalid_count"},

		{"reorg: empty body", "/sim/reorg", ``, "invalid_body"},
		{"reorg: depth as a string", "/sim/reorg", `{"depth":"1"}`, "invalid_body"},
		{"reorg: drop_transfers as a string", "/sim/reorg", `{"depth":1,"drop_transfers":"yes"}`, "invalid_body"},
		{"reorg: unknown field", "/sim/reorg", `{"depth":1,"drop":true}`, "invalid_body"},
		{"reorg: no depth", "/sim/reorg", `{}`, "invalid_depth"},
		{"reorg: zero", "/sim/reorg", `{"depth":0}`, "invalid_depth"},
		{"reorg: negative", "/sim/reorg", `{"depth":-2}`, "invalid_depth"},
		{"reorg: deeper than the chain", "/sim/reorg", `{"depth":1}`, "reorg_too_deep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.do(http.MethodPost, tt.path, "", tt.body), http.StatusBadRequest, tt.code)
		})
	}

	// Ни один отклонённый запрос не изменил сеть.
	head, err := f.api.sim.Head(context.Background())
	if err != nil || head != 0 {
		t.Errorf("head = %d, %v after rejected requests, want 0", head, err)
	}
	f.mine(1)
	rec := f.do(http.MethodPost, "/sim/reorg", "", `{"depth":1}`)
	wantStatus(t, rec, http.StatusOK) // а на цепочке из одного блока глубина 1 допустима
}
