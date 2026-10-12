package e2e_test

import "testing"

// Деньги с адреса из чёрного списка партнёру не достаются: платёж отклоняется, а отправителю
// создаётся возврат на всю сумму. Возврат один, сколько бы проходов и перезапусков ни было.
func TestHighRiskPaymentIsRejectedAndRefundedOnce(t *testing.T) {
	e := newEnv(t)
	inv := e.alpha.createInvoice("order-1", "100")

	// Перевод прошёл порог подтверждений и ждёт ответа AML: деньги в холде.
	e.transfer(dirtySender, inv.DepositAddress, "100")
	e.mine(confirmations)
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
	e.wantBooks(books{Deposits: e.usdt("100"), Held: e.usdt("100")})

	// AML ответил «высокий риск»: платёж отклонён, счёт не оплачен, а на адрес отправителя
	// создан возврат на полную сумму, без вычета комиссии. Возврат уже готов к отправке.
	e.process()
	got := e.alpha.wantInvoice(inv.ID, "pending", zero, "rejected")
	want := refund{Status: "ready_to_send", ToAddress: dirtySender, Amount: "100.000000"}
	if r := onlyPayment(t, got).Refund; r == nil || *r != want {
		t.Fatalf("refund = %+v, want %+v", r, want)
	}
	wantRefundedOnce := func() {
		t.Helper()
		e.alpha.wantBalance(zero)
		e.beta.wantBalance(zero)
		e.wantBooks(books{Deposits: e.usdt("100"), Refunds: e.usdt("100")})
		e.wantPostings(map[string]int{kindHold: 1, kindRefund: 1})
		if n := e.count(`SELECT count(*) FROM refunds.refunds`); n != 1 {
			t.Fatalf("refunds in the database = %d, want 1", n)
		}
	}
	wantRefundedOnce()

	// Повторные проходы и новые экземпляры сервиса не создают второй возврат и не двигают ledger.
	e.process()
	e.scan()
	e.process()
	e.restart()
	e.process()
	e.scan()
	e.process()
	got = e.alpha.wantInvoice(inv.ID, "pending", zero, "rejected")
	if r := onlyPayment(t, got).Refund; r == nil || *r != want {
		t.Fatalf("refund after repeated passes = %+v, want %+v", r, want)
	}
	wantRefundedOnce()

	// Второй такой же перевод с того же адреса — другой платёж и свой возврат: один платёж,
	// один возврат.
	e.transfer(dirtySender, inv.DepositAddress, "100")
	e.mine(confirmations)
	e.scan()
	e.process()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "rejected", "rejected")
	e.wantBooks(books{Deposits: e.usdt("200"), Refunds: e.usdt("200")})
	e.wantPostings(map[string]int{kindHold: 2, kindRefund: 2})
	if n := e.count(`SELECT count(*) FROM refunds.refunds`); n != 2 {
		t.Fatalf("refunds in the database = %d, want 2: one per payment", n)
	}
}

// Пока AML недоступен, платёж не зачисляется и не отклоняется: он ждёт в холде и проверяется снова.
// Когда AML вернулся, платёж зачислен. Пауза между попытками в тесте не ждётся (см. makeDue).
func TestAMLOutageKeepsPaymentOnHoldUntilAMLReturns(t *testing.T) {
	e := newEnv(t)
	inv := e.alpha.createInvoice("order-1", "100")
	e.setAML(false)

	e.transfer(cleanSender, inv.DepositAddress, "100")
	e.mine(confirmations)
	e.scan()

	// Сколько бы раз ни пробовали проверить, ответа нет: платёж на проверке, деньги в холде,
	// у партнёра ноль, а ошибкой для сервиса это не считается.
	for range 5 {
		e.makeDue()
		e.process()
		e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
		e.alpha.wantBalance(zero)
		e.wantBooks(books{Deposits: e.usdt("100"), Held: e.usdt("100")})
		e.wantPostings(map[string]int{kindHold: 1})
	}

	// Сервис перезапустился, а AML всё ещё лежит: платёж по-прежнему ждёт.
	e.restart()
	e.makeDue()
	e.process()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
	e.alpha.wantBalance(zero)

	// AML вернулся: следующая проверка проходит, платёж зачислен за вычетом комиссии.
	e.setAML(true)
	e.makeDue()
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})
}
