package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
)

// Тесты итога AML в ответе счёта и переключателя /sim/aml. Время не ждём: сеть двигают ручки
// /sim/*, сканер и обработчик платежей вызываются напрямую.

// makeDue делает платежи на проверке готовыми к ней сейчас. Обработчик работает по настоящим
// часам, а после неудачной попытки платёж ждёт паузу в несколько секунд; тест эту паузу не ждёт,
// а объявляет истёкшей. Саму паузу проверяют тесты пакета payments на подменённых часах.
func (f *fixture) makeDue() {
	f.t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`UPDATE payments.payments SET next_attempt_at = to_timestamp(0) WHERE status = 'aml_pending'`)
	if err != nil {
		f.t.Fatal(err)
	}
}

// process делает проход обработчика платежей.
func (f *fixture) process() {
	f.t.Helper()
	if err := f.proc.Tick(context.Background()); err != nil {
		f.t.Fatalf("payment processor Tick: %v", err)
	}
}

// settle проводит перевод от from через сеть до решения AML: перевод, три блока (порог), проход
// сканера, проход обработчика платежей.
func (f *fixture) settle(from, to, amount string) {
	f.t.Helper()
	f.transferFrom(from, to, amount)
	f.mine(3)
	f.tick()
	f.makeDue()
	f.process()
}

// balance читает баланс партнёра через API.
func (f *fixture) balance(apiKey string) string {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/balance", apiKey, "")
	wantStatus(f.t, rec, http.StatusOK)
	return decode[struct {
		Balance string `json:"balance"`
	}](f.t, rec).Balance
}

// Чистый перевод: платёж credited, счёт paid, партнёр получил сумму за вычетом комиссии,
// баланс другого партнёра не изменился.
func TestInvoiceShowsCreditedPayment(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody)) // счёт на 100, alpha: 1,5%

	f.settle(sender, inv.DepositAddress, "100")

	rec := f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, "")
	got := decode[invoiceWithPayments](t, rec)
	if got.Status != "paid" || got.AmountReceived != "100.000000" {
		t.Errorf("status = %q, amount_received = %q; want paid and 100.000000 (fee is not subtracted)", got.Status, got.AmountReceived)
	}
	if len(got.Payments) != 1 || got.Payments[0].Status != "credited" || got.Payments[0].Amount != "100.000000" {
		t.Fatalf("payments = %+v, want one credited payment of 100.000000", got.Payments)
	}
	if got.Payments[0].Refund != nil || !strings.Contains(rec.Body.String(), `"refund":null`) {
		t.Errorf("body = %s, want refund null for a credited payment", rec.Body)
	}

	if bal := f.balance(keyAlpha); bal != "98.500000" {
		t.Errorf("alpha balance = %s, want 98.500000: 100 minus the 1.5%% fee", bal)
	}
	if bal := f.balance(keyBeta); bal != "0.000000" {
		t.Errorf("beta balance = %s, want 0.000000: another partner's payment must not touch it", bal)
	}
	if err := ledger.CheckBalanced(context.Background(), f.pool); err != nil {
		t.Error(err)
	}
}

// Перевод с адреса из чёрного списка: платёж rejected, возврат на адрес отправителя в
// ready_to_send, партнёру ничего. Повторные проходы второй возврат не создают.
func TestInvoiceShowsRejectedPaymentAndRefund(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	f.settle(dirtySender, inv.DepositAddress, "100")
	f.makeDue()
	f.process()
	f.process()

	got := f.invoice(keyAlpha, inv.ID)
	if len(got.Payments) != 1 || got.Payments[0].Status != "rejected" {
		t.Fatalf("payments = %+v, want one rejected payment", got.Payments)
	}
	wantRefund := refundRow{Status: "ready_to_send", ToAddress: dirtySender, Amount: "100.000000"}
	if r := got.Payments[0].Refund; r == nil || *r != wantRefund {
		t.Errorf("refund = %+v, want %+v", r, wantRefund)
	}
	if got.Status != "pending" || got.AmountReceived != "0.000000" {
		t.Errorf("status = %q, amount_received = %q; want pending and 0.000000: a rejected payment does not pay the invoice",
			got.Status, got.AmountReceived)
	}
	if bal := f.balance(keyAlpha); bal != "0.000000" {
		t.Errorf("alpha balance = %s, want 0.000000: rejected money never reaches the partner", bal)
	}

	totals, err := ledger.Totals(context.Background(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if payable := totals[ledger.AccountRefundsPayable]; payable != -money.Amount(100_000_000) {
		t.Errorf("refunds:payable = %s, want -100.000000 (one refund)", payable)
	}
	if err := ledger.CheckBalanced(context.Background(), f.pool); err != nil {
		t.Error(err)
	}
}

// /sim/aml выключает мок: платёж после трёх блоков остаётся на проверке, деньги не на балансе.
// Мок включили — платёж зачислен.
func TestSimAMLSwitch(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	rec := f.do(http.MethodPost, "/sim/aml", "", `{"available":false}`)
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != `{"available":false}` {
		t.Errorf("body = %s, want {\"available\":false}", rec.Body)
	}

	f.settle(sender, inv.DepositAddress, "100")
	got := f.invoice(keyAlpha, inv.ID)
	if len(got.Payments) != 1 || got.Payments[0].Status != "aml_pending" || got.Payments[0].Refund != nil {
		t.Fatalf("payments = %+v, want one aml_pending payment while AML is down", got.Payments)
	}
	if got.Status != "pending" || got.AmountReceived != "0.000000" {
		t.Errorf("status = %q, amount_received = %q; want pending and 0.000000", got.Status, got.AmountReceived)
	}
	if bal := f.balance(keyAlpha); bal != "0.000000" {
		t.Errorf("alpha balance = %s while AML is down, want 0.000000", bal)
	}

	rec = f.do(http.MethodPost, "/sim/aml", "", `{"available":true}`)
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != `{"available":true}` {
		t.Errorf("body = %s, want {\"available\":true}", rec.Body)
	}
	f.makeDue() // проверка повторяется, когда истекла пауза
	f.process()

	got = f.invoice(keyAlpha, inv.ID)
	if got.Payments[0].Status != "credited" || got.Status != "paid" {
		t.Errorf("payment = %q, invoice = %q after AML came back; want credited and paid", got.Payments[0].Status, got.Status)
	}
	if bal := f.balance(keyAlpha); bal != "98.500000" {
		t.Errorf("alpha balance = %s after AML came back, want 98.500000", bal)
	}
}

// Неверный ввод /sim/aml — 400 в общем формате, состояние мока не меняется.
func TestSimAMLBadInput(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"empty body":          ``,
		"not json":            `available=false`,
		"array":               `[]`,
		"no field":            `{}`,
		"null":                `{"available":null}`,
		"string instead bool": `{"available":"false"}`,
		"number instead bool": `{"available":0}`,
		"unknown field":       `{"available":false,"extra":1}`,
		"trailing data":       `{"available":false} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, f.do(http.MethodPost, "/sim/aml", "", body), http.StatusBadRequest, "invalid_body")
		})
	}

	if got, err := f.mock.Check(context.Background(), sender); err != nil || got != aml.VerdictLow {
		t.Errorf("Check after rejected requests = %q, %v; want the mock to stay available", got, err)
	}
}
