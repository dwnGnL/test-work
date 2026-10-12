package e2e_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// concurrencyTimeout — страховка от зависшего теста: на успешном пути не ждём, цикл ожидания
// кончается, когда платежи зачислены.
const concurrencyTimeout = 90 * time.Second

// Десять счетов двух партнёров, переводы на все. Пять горутин одновременно ходят в сеть и базу
// (два сканера и три обработчика платежей — как несколько экземпляров сервиса), пока основной
// поток добывает блоки. Итог тот же, что без гонки: каждый платёж зачислен ровно один раз,
// на платёж по одной проводке каждого шага, деньги сходятся. Запускается и под -race.
func TestConcurrentWorkersCreditEachPaymentOnce(t *testing.T) {
	e := newEnv(t)
	pgtest.WarmUp(t, e.pool)
	pgtest.WarmUp(t, e.svcPool)

	type order struct {
		partner *partnerClient
		amount  string
		inv     invoice
	}
	// Суммы подобраны так, чтобы комиссия делилась без остатка: итоги можно записать числами.
	// alpha (1,5%): 700 USDT, комиссия 10,5, партнёру 689,5. beta (0,5%): 1500 USDT, комиссия 7,5, партнёру 1492,5.
	var orders []*order
	for _, a := range []string{"100", "120", "140", "160", "180"} {
		orders = append(orders, &order{partner: e.alpha, amount: a})
	}
	for _, a := range []string{"100", "200", "300", "400", "500"} {
		orders = append(orders, &order{partner: e.beta, amount: a})
	}

	ctx, cancel := context.WithTimeout(context.Background(), concurrencyTimeout)
	defer cancel()

	workers := []ticker{e.newScanner(), e.newScanner(), e.newProcessor(), e.newProcessor(), e.newProcessor()}
	var (
		wg      sync.WaitGroup
		stop    atomic.Bool
		mu      sync.Mutex
		failure []error
	)
	stopWorkers := func() {
		stop.Store(true)
		wg.Wait()
	}
	defer stopWorkers()

	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if err := w.Tick(ctx); err != nil {
					mu.Lock()
					failure = append(failure, err)
					mu.Unlock()
					return
				}
			}
		}()
	}

	// Пока работники крутятся, основной поток выставляет счета, переводит деньги и добывает блоки:
	// переводы попадают в разные блоки, а порог для каждого набирается в разное время.
	for i, o := range orders {
		o.inv = o.partner.createInvoice("order-"+o.amount, o.amount)
		e.transfer(cleanSender, o.inv.DepositAddress, o.amount)
		if i%2 == 1 {
			e.mine(1)
		}
	}
	e.mine(confirmations)

	// Ждём не по таймеру, а по состоянию базы: все платежи зачислены.
	for e.count(`SELECT count(*) FROM payments.payments WHERE status = 'credited'`) < len(orders) {
		if ctx.Err() != nil {
			stopWorkers()
			t.Fatalf("payments were not credited in %s; credited %d of %d, worker errors: %v",
				concurrencyTimeout, e.count(`SELECT count(*) FROM payments.payments WHERE status = 'credited'`),
				len(orders), errors.Join(failure...))
		}
		runtime.Gosched()
	}
	stopWorkers()

	if err := errors.Join(failure...); err != nil {
		t.Fatalf("a worker Tick failed: %v", err)
	}

	// Ещё по проходу каждого вида после гонки: зачисленное не зачисляется второй раз.
	e.scan()
	e.process()

	for _, o := range orders {
		got := o.partner.wantInvoice(o.inv.ID, "paid", e.usdt(o.amount).String(), "credited")
		if pay := onlyPayment(t, got); pay.Amount != e.usdt(o.amount).String() {
			t.Errorf("invoice %s: payment amount = %s, want %s", o.inv.ID, pay.Amount, e.usdt(o.amount))
		}
	}
	if n := e.count(`SELECT count(*) FROM payments.payments`); n != len(orders) {
		t.Errorf("payments in the database = %d, want %d: one per transfer", n, len(orders))
	}
	e.alpha.wantBalance("689.500000")
	e.beta.wantBalance("1492.500000")
	e.wantBooks(books{Deposits: e.usdt("2200"), Fees: e.usdt("18")})
	e.wantPostings(map[string]int{kindHold: len(orders), kindCredit: len(orders)})
}
