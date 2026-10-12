package scanner_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/internal/scanner"
)

// Перевод на адрес счёта: detected с первого блока, aml_pending после N подтверждений
// и одна проводка hold. До порога в ledger пусто.
func TestDetectsTransferAndConfirmsAtThreshold(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	f.tick() // пустая сеть: делать нечего
	txHash := f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1) // блок 1: одно подтверждение
	f.tick()

	p := f.onlyPayment(inv)
	if p.Status != payments.StatusDetected || p.TxHash != txHash || p.LogIndex != 0 || p.Amount != amt(t, "25") ||
		p.From != sender || p.FeeBps != inv.FeeBps || p.PartnerID != inv.PartnerID ||
		p.BlockNumber == nil || *p.BlockNumber != 1 {
		t.Fatalf("payment = %+v, want detected in block 1", p)
	}
	f.assertHold(0)

	f.mine(1) // голова 2: два подтверждения, мало
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusDetected {
		t.Errorf("status with 2 confirmations = %s, want detected", got.Status)
	}
	f.assertHold(0)

	f.mine(1) // голова 3: три подтверждения
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusAMLPending || got.ID != p.ID {
		t.Errorf("payment with 3 confirmations = %+v, want the same payment in aml_pending", got)
	}
	f.assertHold(amt(t, "25"))

	// Лишние проходы ничего не меняют.
	f.tick()
	f.tick()
	f.assertHold(amt(t, "25"))
	f.assertCursor(3)

	received, inProgress, err := payments.Summary(context.Background(), f.pool, inv.ID)
	if err != nil || received != 0 || !inProgress {
		t.Errorf("Summary = %s, %v, %v; want nothing received and a payment in progress", received, inProgress, err)
	}
}

// Реорг до порога, транзакция исчезла: платёж orphaned, проводок нет.
func TestReorgBeforeThresholdDropsPayment(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(2) // блоки 1 и 2; блок 1 с переводом
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusDetected {
		t.Fatalf("status = %s, want detected", got.Status)
	}

	f.reorg(2, true) // оба блока выброшены, транзакция пропала совсем
	f.tick()

	p := f.onlyPayment(inv)
	if p.Status != payments.StatusOrphaned || p.BlockNumber != nil || p.BlockHash != nil {
		t.Errorf("payment = %+v, want orphaned without a block", p)
	}
	f.assertHold(0)

	// Новые блоки платёж не возвращают: перевода в сети больше нет.
	f.mine(5)
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusOrphaned {
		t.Errorf("status after new blocks = %s, want orphaned", got.Status)
	}
	f.assertHold(0)
	f.assertCursor(5)

	if f.logContains("level=ERROR") {
		t.Errorf("a reorg before the threshold must not log an error:\n%s", f.logs.String())
	}
}

// Реорг до порога, транзакцию включили заново: тот же платёж снова detected с новым хэшем блока;
// второй записи нет; после порога одна проводка.
func TestReorgBeforeThresholdReincludesPayment(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	txHash := f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(2)
	f.tick()
	before := f.onlyPayment(inv)
	if before.Status != payments.StatusDetected {
		t.Fatalf("status = %s, want detected", before.Status)
	}

	f.reorg(2, false) // транзакция вернулась в мемпул
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusOrphaned {
		t.Fatalf("status after the reorg = %s, want orphaned", got.Status)
	}
	f.assertHold(0)

	f.mine(1) // новый блок 1 с той же транзакцией, но другим хэшем
	f.tick()
	after := f.onlyPayment(inv)
	if after.ID != before.ID || after.TxHash != txHash || after.Status != payments.StatusDetected {
		t.Fatalf("payment = %+v, want the same payment %d detected again", after, before.ID)
	}
	if after.BlockNumber == nil || *after.BlockNumber != 1 || after.BlockHash == nil || *after.BlockHash == *before.BlockHash {
		t.Errorf("block = %v %v, want block 1 with a new hash (was %s)", after.BlockNumber, after.BlockHash, *before.BlockHash)
	}
	f.assertHold(0)

	f.mine(2)
	f.tick()
	if got := f.onlyPayment(inv); got.ID != before.ID || got.Status != payments.StatusAMLPending {
		t.Errorf("payment after the threshold = %+v, want aml_pending", got)
	}
	f.assertHold(amt(t, "25"))
	f.assertCursor(3)

	if f.logContains("level=ERROR") {
		t.Errorf("a reorg before the threshold must not log an error:\n%s", f.logs.String())
	}
}

// Реорг глубже порога: подтверждённый платёж не тронут, в лог пишется ошибка.
func TestReorgBeyondThresholdLeavesConfirmedPayment(t *testing.T) {
	for _, tt := range []struct {
		name string
		drop bool
	}{
		{"transaction dropped", true},
		{"transaction included again", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			inv := f.newInvoice()

			f.send(usdtTo(t, inv.DepositAddress, "25"))
			f.mine(3)
			f.tick()
			before := f.onlyPayment(inv)
			if before.Status != payments.StatusAMLPending {
				t.Fatalf("status = %s, want aml_pending", before.Status)
			}
			f.assertHold(amt(t, "25"))
			if f.logContains("level=ERROR") {
				t.Fatal("an error was logged before the reorg")
			}

			f.reorg(3, tt.drop) // выброшен и блок с переводом, он уже за порогом
			f.mine(1)
			f.tick()

			after := f.onlyPayment(inv)
			if !reflect.DeepEqual(after, before) {
				t.Errorf("confirmed payment changed: %+v, was %+v", after, before)
			}
			f.assertHold(amt(t, "25"))
			if !f.logContains("level=ERROR") || !f.logContains("deeper than the confirmation threshold") {
				t.Errorf("no error about the deep reorg in the log:\n%s", f.logs.String())
			}
			f.assertCursor(1)
		})
	}
}

// Рестарт: новый Scanner на той же базе продолжает с курсора; блоки, добытые пока сервис
// лежал, обрабатываются.
func TestRestartContinuesFromCursor(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	f.mine(2)
	f.tick()
	f.assertCursor(2)

	// Сервис остановлен: сеть живёт, переводы и блоки появляются.
	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(3)

	restarted := f.newScanner(f.sim)
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertCursor(5)
	if got := f.onlyPayment(inv); got.Status != payments.StatusAMLPending || *got.BlockNumber != 3 {
		t.Errorf("payment = %+v, want aml_pending in block 3", got)
	}
	f.assertHold(amt(t, "25"))
}

// Повторная обработка: стёртая запись о блоке не задваивает платёж и проводку.
func TestReprocessingDoesNotDuplicate(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()
	ctx := context.Background()

	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1)
	f.tick()
	first := f.onlyPayment(inv)

	// Стираем запись о последнем блоке: следующий проход прочтёт его заново.
	if _, err := f.pool.Exec(ctx, `DELETE FROM scanner.blocks WHERE number = (SELECT max(number) FROM scanner.blocks)`); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if got := f.onlyPayment(inv); !reflect.DeepEqual(got, first) {
		t.Errorf("payment after reprocessing = %+v, want %+v", got, first)
	}
	f.assertCursor(1)

	// То же после порога: курсор стёрт целиком, сканер проходит всю сеть заново.
	f.mine(2)
	f.tick()
	if got := f.onlyPayment(inv); got.Status != payments.StatusAMLPending {
		t.Fatalf("status = %s, want aml_pending", got.Status)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM scanner.blocks`); err != nil {
		t.Fatal(err)
	}
	f.tick()
	f.tick()
	if got := f.onlyPayment(inv); got.ID != first.ID || got.Status != payments.StatusAMLPending {
		t.Errorf("payment after a full rescan = %+v, want the same payment in aml_pending", got)
	}
	f.assertHold(amt(t, "25"))
	f.assertCursor(3)
}

// Поддельный токен, нулевая сумма и чужой получатель игнорируются; настоящий перевод
// из той же транзакции принимается со своим log_index.
func TestIgnoresForeignTransfers(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()
	to := inv.DepositAddress

	f.send(
		chain.TransferSpec{Token: fakeToken, From: sender, To: to, Amount: amt(t, "50")}, // 0: поддельный токен
		usdtTo(t, to, "0"),        // 1: нулевая сумма
		usdtTo(t, stranger, "50"), // 2: чужой получатель
		usdtTo(t, to, "7"),        // 3: настоящий перевод
		chain.TransferSpec{Token: "0x" + strings.ToUpper(usdt[2:]), From: sender, To: to, Amount: amt(t, "1")}, // 4: тот же USDT, адрес записан иначе
	)
	f.mine(1)
	f.tick()

	list := f.payments(inv)
	if len(list) != 2 {
		t.Fatalf("payments = %+v, want two (log_index 3 and 4)", list)
	}
	if list[0].LogIndex != 3 || list[0].Amount != amt(t, "7") || list[1].LogIndex != 4 || list[1].Amount != amt(t, "1") {
		t.Errorf("payments = %+v, want events 3 (7 USDT) and 4 (1 USDT)", list)
	}
	f.assertHold(0)
}

// Одна транзакция с двумя событиями на два разных счёта — два платежа с одним tx_hash.
func TestOneTransactionTwoInvoices(t *testing.T) {
	f := newFixture(t)
	a, b := f.newInvoice(), f.newInvoice()

	txHash := f.send(usdtTo(t, a.DepositAddress, "10"), usdtTo(t, b.DepositAddress, "20"))
	f.mine(1)
	f.tick()

	pa, pb := f.onlyPayment(a), f.onlyPayment(b)
	if pa.TxHash != txHash || pb.TxHash != txHash || pa.LogIndex != 0 || pb.LogIndex != 1 ||
		pa.Amount != amt(t, "10") || pb.Amount != amt(t, "20") || pa.PartnerID == pb.PartnerID {
		t.Errorf("payments = %+v and %+v, want events 0 and 1 of %s on different partners", pa, pb, txHash)
	}

	f.mine(2)
	f.tick()
	f.assertHold(amt(t, "30"))
}

// Два перевода на один счёт — два платежа: из разных транзакций и из одной.
func TestTwoTransfersOneInvoice(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	f.send(usdtTo(t, inv.DepositAddress, "10"))
	f.send(usdtTo(t, inv.DepositAddress, "20"))
	f.send(usdtTo(t, inv.DepositAddress, "1"), usdtTo(t, inv.DepositAddress, "2"))
	f.mine(3)
	f.tick()

	list := f.payments(inv)
	if len(list) != 4 {
		t.Fatalf("payments = %+v, want four", list)
	}
	f.assertHold(amt(t, "33"))
	for _, p := range list {
		if p.Status != payments.StatusAMLPending {
			t.Errorf("payment %+v, want aml_pending", p)
		}
	}
}

// Блок обрабатывается целиком или не обрабатывается вовсе: ошибка на втором переводе
// откатывает и первый платёж, и запись о блоке.
func TestBlockIsAtomic(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()

	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1)

	broken := true
	network := &hookChain{Chain: f.sim, onTransfers: func(got []chain.Transfer) []chain.Transfer {
		if !broken {
			return got
		}
		// Второй перевод блока с недопустимым log_index: Detect его отклонит.
		bad := chain.Transfer{TxHash: "0xbad", LogIndex: -1, Token: usdt, From: sender, To: inv.DepositAddress, Amount: 1}
		return append(got, bad)
	}}
	sc := f.newScanner(network)

	err := sc.Tick(context.Background())
	if !errors.Is(err, payments.ErrInvalidParams) {
		t.Fatalf("Tick error = %v, want ErrInvalidParams", err)
	}
	if list := f.payments(inv); len(list) != 0 {
		t.Errorf("payments = %+v after a failed block, want none: the valid transfer must roll back", list)
	}
	if blocks := f.scannedBlocks(); len(blocks) != 0 {
		t.Errorf("scanner blocks = %+v after a failed block, want none: the cursor must not move", blocks)
	}

	// Сеть починилась: тот же блок обрабатывается целиком.
	broken = false
	if err := sc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.onlyPayment(inv); got.Status != payments.StatusDetected {
		t.Errorf("payment = %+v, want detected", got)
	}
	f.assertCursor(1)
}

// Ошибка сети при чтении событий блока: ни блока, ни платежа, следующий проход повторяет.
func TestNetworkErrorLeavesBlockUnprocessed(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()
	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1)

	// Сеть «не знает» блок — как eth_getLogs по выброшенному блоку.
	sc := f.newScanner(unknownBlockChain{f.sim})
	if err := sc.Tick(context.Background()); !errors.Is(err, chain.ErrBlockNotFound) {
		t.Fatalf("Tick error = %v, want ErrBlockNotFound", err)
	}
	if blocks := f.scannedBlocks(); len(blocks) != 0 {
		t.Errorf("scanner blocks = %+v, want none", blocks)
	}

	f.tick()
	f.onlyPayment(inv)
	f.assertCursor(1)
}

// unknownBlockChain отвечает на запрос событий так, будто блок выброшен.
type unknownBlockChain struct{ chain.Chain }

func (unknownBlockChain) Transfers(_ context.Context, blockHash string) ([]chain.Transfer, error) {
	return nil, chain.ErrBlockNotFound
}

// Сеть переписала цепочку между чтением головы и чтением блока: parent_hash нового блока
// не сошёлся с нашим, сканер возвращается к сверке и за тот же проход приходит к новой цепочке.
func TestParentHashMismatchRestartsReconciliation(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()
	ctx := context.Background()

	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1)
	f.tick() // блок 1 обработан, платёж detected
	before := f.onlyPayment(inv)
	f.mine(1) // блок 2 на старой цепочке

	rewritten := false
	network := &hookChain{Chain: f.sim, onBlock: func(number int64) {
		if number != 2 || rewritten {
			return
		}
		rewritten = true
		// Пока сканер читает блок 2, сеть заменяет блоки 1 и 2 другими; транзакция остаётся в новом блоке 1.
		if _, err := f.sim.Reorg(ctx, 2, false); err != nil {
			t.Error(err)
		}
		if _, err := f.sim.Mine(ctx, 2); err != nil {
			t.Error(err)
		}
	}}
	sc := f.newScanner(network)
	if err := sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !rewritten {
		t.Fatal("the test did not rewrite the chain")
	}

	after := f.onlyPayment(inv)
	if after.ID != before.ID || after.Status != payments.StatusDetected || *after.BlockNumber != 1 || *after.BlockHash == *before.BlockHash {
		t.Errorf("payment = %+v, want the same payment in the new block 1 (was %+v)", after, before)
	}
	f.assertCursor(2)
}

// Если сеть переписывает цепочку при каждом чтении, проход заканчивается ошибкой, а не зацикливается.
func TestUnstableChainStopsThePass(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.mine(3)
	f.tick()

	f.mine(1) // блок 4; на его чтении сеть каждый раз подменяет блоки 3 и 4
	network := &hookChain{Chain: f.sim, onBlock: func(number int64) {
		if number != 4 {
			return
		}
		if _, err := f.sim.Reorg(ctx, 2, false); err != nil {
			t.Error(err)
		}
		if _, err := f.sim.Mine(ctx, 2); err != nil {
			t.Error(err)
		}
	}}
	sc := f.newScanner(network)
	if err := sc.Tick(ctx); !errors.Is(err, scanner.ErrChainUnstable) {
		t.Errorf("Tick error = %v, want ErrChainUnstable", err)
	}
}

// Второй экземпляр пропускает проход, пока первый работает: без ошибки и без обработки блоков.
func TestSecondScannerSkipsTheTick(t *testing.T) {
	f := newFixture(t)
	inv := f.newInvoice()
	ctx := context.Background()
	f.send(usdtTo(t, inv.DepositAddress, "25"))
	f.mine(1)

	second := f.newScanner(f.sim)
	nested := false
	network := &hookChain{Chain: f.sim, onHead: func() {
		if nested {
			return
		}
		nested = true
		// Первый сканер уже держит блокировку и читает сеть: второй проход должен уйти молча.
		if err := second.Tick(ctx); err != nil {
			t.Errorf("second Tick error = %v, want nil", err)
		}
		if blocks := f.scannedBlocks(); len(blocks) != 0 {
			t.Errorf("the second scanner processed blocks while the first held the lock: %+v", blocks)
		}
	}}
	first := f.newScanner(network)
	if err := first.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !nested {
		t.Fatal("the nested Tick did not run")
	}
	f.onlyPayment(inv)
	f.assertCursor(1)

	// Блокировка снята: второй экземпляр снова может работать.
	f.mine(1)
	if err := second.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.assertCursor(2)
}

// Два сканера на одной базе, Tick из нескольких горутин, пока добываются блоки: ни дубля платежа,
// ни дубля проводки, курсор согласован с сетью.
func TestTwoScannersConcurrently(t *testing.T) {
	f := newFixture(t)
	pgtest.WarmUp(t, f.pool)
	ctx := context.Background()

	invs := []invoices.Invoice{f.newInvoice(), f.newInvoice(), f.newInvoice()}
	scanners := []*scanner.Scanner{f.sc, f.newScanner(f.sim)}

	// Переводы на 1, 2, ... 30 USDT по кругу на три счёта: сумма проводок говорит, сколько платежей проведено.
	const transfers, tickers, ticksEach = 30, 4, 25
	specs := make([]chain.TransferSpec, transfers)
	var total money.Amount
	for i := range specs {
		amount := money.Amount(i+1) * amt(t, "1")
		specs[i] = chain.TransferSpec{Token: usdt, From: sender, To: invs[i%len(invs)].DepositAddress, Amount: amount}
		total += amount
	}

	errs := make(chan error, 2*transfers+tickers*ticksEach)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // сеть: перевод и сразу блок
		defer wg.Done()
		for _, spec := range specs {
			_, err := f.sim.SubmitTx(ctx, []chain.TransferSpec{spec})
			errs <- err
			_, err = f.sim.Mine(ctx, 1)
			errs <- err
		}
	}()
	for i := range tickers { // сканеры: по два Tick-цикла на экземпляр
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ticksEach {
				errs <- scanners[i%len(scanners)].Tick(ctx)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call: %v", err)
		}
	}

	// Добираем подтверждения и делаем последний проход: все платежи за порогом.
	f.mine(confirmations)
	f.tick()

	count := 0
	for _, inv := range invs {
		for _, p := range f.payments(inv) {
			count++
			if p.Status != payments.StatusAMLPending {
				t.Errorf("payment %+v, want aml_pending", p)
			}
		}
	}
	if count != transfers {
		t.Errorf("payments = %d, want %d: one per transfer", count, transfers)
	}
	f.assertHold(total)
	f.assertCursor(transfers + confirmations)
}
