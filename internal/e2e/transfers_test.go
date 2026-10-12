package e2e_test

import "testing"

// Недоплата, доплата и переплата. Счёт на 100: сначала пришло 60 (оплачен частично), потом ещё 40
// (оплачен). Другой счёт на 100, на который пришло 120: оплачен, а партнёру зачислена вся
// полученная сумма за вычетом комиссии с неё.
func TestUnderAndOverpayment(t *testing.T) {
	e := newEnv(t)
	underpaid := e.alpha.createInvoice("order-1", "100")
	overpaid := e.alpha.createInvoice("order-2", "100")

	// 60 из 100: комиссия 1,5% с 60 — 0,9, партнёру 59,1.
	e.transfer(cleanSender, underpaid.DepositAddress, "60")
	e.mine(confirmations)
	e.scan()
	e.process()
	e.alpha.wantInvoice(underpaid.ID, "partially_paid", "60.000000", "credited")
	e.alpha.wantBalance("59.100000")

	// Ещё 40: комиссия 0,6, партнёру 39,4. Всего получено 100, счёт оплачен.
	e.transfer(cleanSender, underpaid.DepositAddress, "40")
	e.mine(confirmations)
	e.scan()
	e.process()
	e.alpha.wantInvoice(underpaid.ID, "paid", "100.000000", "credited", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})

	// 120 на счёт в 100: комиссия 1,8 с полной суммы, партнёру 118,2. К прежним 98,5 вместе 216,7.
	e.transfer(cleanSender, overpaid.DepositAddress, "120")
	e.mine(confirmations)
	e.scan()
	e.process()
	e.alpha.wantInvoice(overpaid.ID, "paid", "120.000000", "credited")
	e.alpha.wantBalance("216.700000")
	e.beta.wantBalance(zero)
	e.wantBooks(books{Deposits: e.usdt("220"), Fees: e.usdt("3.3")})
	e.wantPostings(map[string]int{kindHold: 3, kindCredit: 3})
}

// Перевод «USDT» с другим адресом контракта на адрес счёта и перевод настоящего USDT на адрес без
// счёта сервис не считает платежами: ни платежей, ни проводок. Настоящий перевод на тот же адрес
// при этом зачисляется, значит, тест не слепой.
func TestFakeTokenAndUnknownAddressAreIgnored(t *testing.T) {
	e := newEnv(t)
	inv := e.alpha.createInvoice("order-1", "100")

	e.transferToken(fakeToken, cleanSender, inv.DepositAddress, "100")
	e.transfer(cleanSender, unknownAddress, "100")
	e.mine(confirmations)
	e.scan()
	e.process()

	e.alpha.wantInvoice(inv.ID, "pending", zero)
	e.alpha.wantBalance(zero)
	e.beta.wantBalance(zero)
	e.wantBooks(books{})
	e.wantPostings(nil)
	if n := e.count(`SELECT count(*) FROM payments.payments`); n != 0 {
		t.Fatalf("payments in the database = %d, want 0", n)
	}

	// Настоящий USDT на тот же адрес: платёж один, и поддельные 100 в сумму не вошли.
	e.transfer(cleanSender, inv.DepositAddress, "100")
	e.mine(confirmations)
	e.scan()
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
}
