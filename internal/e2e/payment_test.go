package e2e_test

import (
	"net/http"
	"testing"
)

// Главный сценарий: партнёр создаёт счёт, клиент платит, сервис ждёт три подтверждения и ответ AML
// и только потом зачисляет партнёру сумму за вычетом комиссии. До ответа AML у партнёра нет ни копейки.
func TestPaymentIsCreditedAfterConfirmationsAndAML(t *testing.T) {
	e := newEnv(t)

	// Партнёр alpha (комиссия 1,5%) создаёт счёт на 100 USDT. Повтор запроса с тем же ключом
	// идемпотентности возвращает тот же счёт, а не создаёт второй.
	inv := e.alpha.createInvoice("order-1", "100")
	if inv.Status != "pending" || inv.Amount != "100.000000" || inv.AmountReceived != zero || len(inv.Payments) != 0 {
		t.Fatalf("new invoice = %+v, want a pending invoice of 100 USDT without payments", inv)
	}
	repeat := e.alpha.createInvoiceRaw("order-1", "100")
	wantStatus(t, repeat, http.StatusOK)
	if again := decode[invoice](t, repeat); again.ID != inv.ID || again.DepositAddress != inv.DepositAddress {
		t.Fatalf("repeated request returned invoice %s at %s, want the same invoice %s at %s",
			again.ID, again.DepositAddress, inv.ID, inv.DepositAddress)
	}
	// Тот же ключ с другой суммой — уже другой запрос, и молча подменить счёт нельзя.
	wantError(t, e.alpha.createInvoiceRaw("order-1", "200"), http.StatusUnprocessableEntity, "idempotency_conflict")

	// Клиент переводит 100 USDT на адрес счёта. Пока перевод в мемпуле и блока нет, сервис его не видит.
	txHash := e.transfer(cleanSender, inv.DepositAddress, "100")
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero)

	// Блок 1 с переводом: одно подтверждение из трёх. Платёж замечен, но денег у партнёра нет
	// и в ledger пусто.
	e.mine(1)
	e.scan()
	pay := onlyPayment(t, e.alpha.wantInvoice(inv.ID, "pending", zero, "detected"))
	if pay.TxHash != txHash || pay.Amount != "100.000000" || pay.BlockNumber == nil || *pay.BlockNumber != 1 {
		t.Fatalf("payment = %+v, want the transfer %s of 100 USDT in block 1", pay, txHash)
	}
	e.alpha.wantBalance(zero)
	e.wantBooks(books{})
	e.wantPostings(nil)

	// Блок 2: два подтверждения всё ещё мало.
	e.mine(1)
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "detected")
	e.wantBooks(books{})

	// Блок 3: порог достигнут. AML ещё не отвечал, поэтому платёж ждёт, 100 USDT лежат в холде,
	// а партнёру не зачислено ничего.
	e.mine(1)
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
	e.alpha.wantBalance(zero)
	e.wantBooks(books{Deposits: e.usdt("100"), Held: e.usdt("100")})
	e.wantPostings(map[string]int{kindHold: 1})

	// Обработчик получил от AML «низкий риск»: платёж зачислен, счёт оплачен. Партнёру достаётся
	// 100 минус 1,5% = 98,5, комиссия 1,5 уходит сервису, холд пуст.
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})

	// Другой партнёр не получил ничего и чужого счёта не видит: для него его просто нет.
	e.beta.wantBalance(zero)
	wantError(t, e.beta.getInvoiceRaw(inv.ID), http.StatusNotFound, "not_found")

	// Повторные проходы ничего не меняют: тот же перевод второй раз не зачисляется.
	e.scan()
	e.process()
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})
}

// Сервис перезапускается после каждого шага: сканер и обработчик каждый раз новые, а курсор,
// очередь платежей и проводки лежат в базе. Блоки, добытые, пока сервис лежал, обрабатываются,
// итог тот же, что без рестартов: один платёж, одно зачисление, две проводки.
func TestScenarioSurvivesServiceRestarts(t *testing.T) {
	e := newEnv(t)

	inv := e.alpha.createInvoice("order-1", "100")
	e.restart()

	e.transfer(cleanSender, inv.DepositAddress, "100")
	e.mine(1)
	e.restart()

	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "detected")
	e.restart()

	// Сервис лежит, а сеть живёт: за это время добыты ещё два блока, порог пройден.
	e.mine(2)
	e.restart()

	// Новый сканер продолжает с курсора из базы, читает пропущенные блоки и подтверждает платёж.
	e.scan()
	e.alpha.wantInvoice(inv.ID, "pending", zero, "aml_pending")
	e.alpha.wantBalance(zero)
	e.wantBooks(books{Deposits: e.usdt("100"), Held: e.usdt("100")})
	e.restart()

	// Новый обработчик берёт платёж из очереди и зачисляет его.
	e.process()
	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.restart()

	// Повторные проходы новых экземпляров ничего не дублируют.
	e.scan()
	e.process()
	e.restart()
	e.scan()
	e.process()

	e.alpha.wantInvoice(inv.ID, "paid", "100.000000", "credited")
	e.alpha.wantBalance("98.500000")
	e.beta.wantBalance(zero)
	e.wantBooks(books{Deposits: e.usdt("100"), Fees: e.usdt("1.5")})
	e.wantPostings(map[string]int{kindHold: 1, kindCredit: 1})
}
