package e2e_test

import "testing"

// Откат блока до набора трёх подтверждений, транзакция из сети исчезла: платёж помечается
// orphaned и не зачисляется никогда. «Висящих» денег нет: проводок не было и нет.
func TestReorgBeforeThresholdLeavesNoCredit(t *testing.T) {
	e := newEnv(t)
	inv := e.alpha.createInvoice("order-1", "100")

	// Перевод в блоке 1, ещё один блок сверху: два подтверждения из трёх.
	e.transfer(cleanSender, inv.DepositAddress, "100")
	e.mine(2)
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "detected")

	// Оба блока выброшены, перевод пропал из сети совсем.
	if head := e.reorg(2, true); head != 0 {
		t.Fatalf("head after the reorg = %d, want 0: the whole chain is gone", head)
	}
	e.scan()
	got := e.alpha.wantInvoice(inv.ID, "pending", zero, "orphaned")
	if pay := onlyPayment(t, got); pay.BlockNumber != nil {
		t.Fatalf("orphaned payment is still in block %d, want no block", *pay.BlockNumber)
	}

	// Сеть живёт дальше, блоков с избытком над порогом, но перевода в них нет: платёж так и остаётся
	// откатившимся, а ни сканер, ни обработчик ничего не проводят.
	for range 3 {
		e.mine(confirmations)
		e.scan()
		e.process()
		e.alpha.wantInvoice(inv.ID, "pending", zero, "orphaned")
		e.alpha.wantBalance(zero)
		e.wantBooks(books{})
		e.wantPostings(nil)
	}
}

// Откат блока до порога, но транзакцию включили в новый блок. Это тот же платёж, а не второй:
// он снова detected, добирает подтверждения и зачисляется ровно один раз.
func TestReorgBeforeThresholdThenReincludedIsCreditedOnce(t *testing.T) {
	e := newEnv(t)
	inv := e.alpha.createInvoice("order-1", "100")

	e.transfer(cleanSender, inv.DepositAddress, "100")
	e.mine(2)
	e.scan()
	first := onlyPayment(t, e.alpha.wantInvoice(inv.ID, "pending", zero, "detected"))

	// Блоки выброшены, а перевод вернулся в мемпул. Платёж откатился.
	e.reorg(2, false)
	e.scan()
	got := e.alpha.wantInvoice(inv.ID, "pending", zero, "orphaned")
	if pay := onlyPayment(t, got); pay.TxHash != first.TxHash || pay.BlockNumber != nil {
		t.Fatalf("orphaned payment = %+v, want the same transfer %s without a block", pay, first.TxHash)
	}
	e.wantBooks(books{})
	e.wantPostings(nil)

	// Новый блок снова включил ту же транзакцию: тот же платёж опять detected, счёт всё ещё
	// с одним платежом, зачислений нет.
	e.mine(1)
	e.scan()
	again := onlyPayment(t, e.alpha.wantInvoice(inv.ID, "pending", zero, "detected"))
	if again.TxHash != first.TxHash || again.BlockNumber == nil || *again.BlockNumber != 1 {
		t.Fatalf("payment after re-inclusion = %+v, want the same transfer %s in block 1", again, first.TxHash)
	}
	e.wantBooks(books{})

	// Порог набран заново: платёж проходит AML и зачисляется один раз.
	e.mine(2)
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})

	e.scan()
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})
}
