package chain_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

const (
	usdt  = "0xdac17f958d2ee523a2206206994597c13d831ec7"
	alice = "0x1111111111111111111111111111111111111111"
	bob   = "0x2222222222222222222222222222222222222222"
)

var (
	hashRe   = regexp.MustCompile(`^0x[0-9a-f]{64}$`)
	zeroHash = "0x" + strings.Repeat("0", 64)
)

// newSim создаёт пустую базу с миграциями и симулятор на ней.
func newSim(t *testing.T) (*chain.Simulator, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewMigrated(t)
	return chain.NewSimulator(pool), pool
}

func amt(t testing.TB, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// spec — перевод USDT на адрес to.
func spec(t testing.TB, to, amount string) chain.TransferSpec {
	t.Helper()
	return chain.TransferSpec{Token: usdt, From: alice, To: to, Amount: amt(t, amount)}
}

func mine(t testing.TB, sim *chain.Simulator, count int) chain.Block {
	t.Helper()
	head, err := sim.Mine(context.Background(), count)
	if err != nil {
		t.Fatalf("Mine(%d): %v", count, err)
	}
	return head
}

func submit(t testing.TB, sim *chain.Simulator, specs ...chain.TransferSpec) string {
	t.Helper()
	hash, err := sim.SubmitTx(context.Background(), specs)
	if err != nil {
		t.Fatalf("SubmitTx: %v", err)
	}
	return hash
}

func block(t testing.TB, sim *chain.Simulator, number int64) chain.Block {
	t.Helper()
	b, found, err := sim.Block(context.Background(), number)
	if err != nil || !found {
		t.Fatalf("Block(%d) = %+v, %v, %v; want the block", number, b, found, err)
	}
	return b
}

func transfers(t testing.TB, sim *chain.Simulator, blockHash string) []chain.Transfer {
	t.Helper()
	got, err := sim.Transfers(context.Background(), blockHash)
	if err != nil {
		t.Fatalf("Transfers(%s): %v", blockHash, err)
	}
	return got
}

func head(t testing.TB, sim *chain.Simulator) int64 {
	t.Helper()
	n, err := sim.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// assertChain проверяет, что блоки 1..n есть, идут подряд и каждый ссылается на хэш предыдущего.
func assertChain(t testing.TB, sim *chain.Simulator, n int64) {
	t.Helper()
	if got := head(t, sim); got != n {
		t.Fatalf("Head = %d, want %d", got, n)
	}
	parent, seen := zeroHash, map[string]bool{}
	for number := int64(1); number <= n; number++ {
		b := block(t, sim, number)
		if b.Number != number || b.ParentHash != parent || !hashRe.MatchString(b.Hash) || seen[b.Hash] {
			t.Fatalf("block %d = %+v, want parent %s and a new hash", number, b, parent)
		}
		parent, seen[b.Hash] = b.Hash, true
	}
	if _, found, err := sim.Block(context.Background(), n+1); err != nil || found {
		t.Fatalf("Block(%d) = found %v, %v; want no such block", n+1, found, err)
	}
}

// Пока блоков нет, голова — 0, а блока с любым номером нет.
func TestEmptyChain(t *testing.T) {
	sim, _ := newSim(t)
	ctx := context.Background()

	if n := head(t, sim); n != 0 {
		t.Errorf("Head = %d, want 0", n)
	}
	for _, number := range []int64{-1, 0, 1} {
		if _, found, err := sim.Block(ctx, number); err != nil || found {
			t.Errorf("Block(%d) = found %v, %v; want no such block", number, found, err)
		}
	}
}

// Блоки идут подряд с единицы, parent_hash ссылается на предыдущий, у первого — нулевой хэш.
func TestMineBuildsChain(t *testing.T) {
	sim, _ := newSim(t)

	first := mine(t, sim, 1)
	if first.Number != 1 || first.ParentHash != zeroHash || !hashRe.MatchString(first.Hash) {
		t.Errorf("first block = %+v", first)
	}

	last := mine(t, sim, 3)
	if last.Number != 4 {
		t.Errorf("Mine(3) returned block %d, want 4", last.Number)
	}
	assertChain(t, sim, 4)
	if got := block(t, sim, 4); got != last {
		t.Errorf("Block(4) = %+v, Mine returned %+v", got, last)
	}
}

func TestMineRejectsBadCount(t *testing.T) {
	sim, _ := newSim(t)
	for _, count := range []int{0, -1, 1001} {
		if _, err := sim.Mine(context.Background(), count); !errors.Is(err, chain.ErrInvalidCount) {
			t.Errorf("Mine(%d) error = %v, want ErrInvalidCount", count, err)
		}
	}
	if n := head(t, sim); n != 0 {
		t.Errorf("Head = %d after rejected calls, want 0", n)
	}
}

// Транзакция с двумя событиями лежит в мемпуле, пока блок не добыт; первый добытый блок
// забирает её целиком, остальные пустые.
func TestMineIncludesMempool(t *testing.T) {
	sim, _ := newSim(t)
	mine(t, sim, 1) // блок 1 пустой

	txHash := submit(t, sim, spec(t, alice, "5"), spec(t, bob, "7.5"))
	if !hashRe.MatchString(txHash) {
		t.Errorf("tx hash = %q, want 0x and 64 lowercase hex digits", txHash)
	}
	if got := transfers(t, sim, block(t, sim, 1).Hash); len(got) != 0 {
		t.Fatalf("a mempool transaction is already in block 1: %+v", got)
	}

	mine(t, sim, 2) // блоки 2 и 3
	want := []chain.Transfer{
		{TxHash: txHash, LogIndex: 0, Token: usdt, From: alice, To: alice, Amount: amt(t, "5")},
		{TxHash: txHash, LogIndex: 1, Token: usdt, From: alice, To: bob, Amount: amt(t, "7.5")},
	}
	got := transfers(t, sim, block(t, sim, 2).Hash)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("block 2 transfers = %+v, want %+v", got, want)
	}
	if got := transfers(t, sim, block(t, sim, 3).Hash); len(got) != 0 {
		t.Errorf("block 3 must be empty, got %+v", got)
	}

	// Мемпул пуст: следующий блок ничего не получает.
	mine(t, sim, 1)
	if got := transfers(t, sim, block(t, sim, 4).Hash); len(got) != 0 {
		t.Errorf("block 4 must be empty, got %+v", got)
	}
}

// Отдельные транзакции получают разные хэши, их события не смешиваются.
func TestSubmitTxHashesAreUnique(t *testing.T) {
	sim, _ := newSim(t)
	a, b := submit(t, sim, spec(t, alice, "1")), submit(t, sim, spec(t, alice, "1"))
	if a == b {
		t.Fatal("two transactions got the same hash")
	}
	mine(t, sim, 1)

	got := transfers(t, sim, block(t, sim, 1).Hash)
	if len(got) != 2 || got[0].TxHash != a || got[1].TxHash != b || got[0].LogIndex != 0 || got[1].LogIndex != 0 {
		t.Errorf("block 1 transfers = %+v, want one event of %s and one of %s, both with log_index 0", got, a, b)
	}
}

// Адреса приводятся к нижнему регистру; нулевая сумма допустима.
func TestSubmitTxLowercasesAddresses(t *testing.T) {
	sim, _ := newSim(t)
	mixed := "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	lower := strings.ToLower(mixed)
	submit(t, sim, chain.TransferSpec{Token: "0x" + strings.ToUpper(usdt[2:]), From: mixed, To: mixed, Amount: 0})
	mine(t, sim, 1)

	got := transfers(t, sim, block(t, sim, 1).Hash)
	if len(got) != 1 || got[0].Token != usdt || got[0].From != lower || got[0].To != lower || got[0].Amount != 0 {
		t.Errorf("transfers = %+v, want lowercase addresses and zero amount", got)
	}
}

func TestSubmitTxRejectsInvalid(t *testing.T) {
	sim, _ := newSim(t)
	ok := spec(t, alice, "1")

	tests := []struct {
		name  string
		specs []chain.TransferSpec
	}{
		{"nil list", nil},
		{"empty list", []chain.TransferSpec{}},
		{"bad token", []chain.TransferSpec{{Token: "usdt", From: alice, To: bob, Amount: 1}}},
		{"short from", []chain.TransferSpec{{Token: usdt, From: "0x12", To: bob, Amount: 1}}},
		{"no 0x in to", []chain.TransferSpec{{Token: usdt, From: alice, To: strings.TrimPrefix(bob, "0x"), Amount: 1}}},
		{"non-hex to", []chain.TransferSpec{{Token: usdt, From: alice, To: "0x" + strings.Repeat("g", 40), Amount: 1}}},
		{"negative amount", []chain.TransferSpec{{Token: usdt, From: alice, To: bob, Amount: -1}}},
		// Плохой второй перевод отклоняет всю транзакцию, первый в мемпул не попадает.
		{"second transfer is bad", []chain.TransferSpec{ok, {Token: usdt, From: alice, To: "nope", Amount: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := sim.SubmitTx(context.Background(), tt.specs)
			if !errors.Is(err, chain.ErrInvalidTransfer) || hash != "" {
				t.Errorf("SubmitTx = %q, %v; want ErrInvalidTransfer", hash, err)
			}
		})
	}

	mine(t, sim, 1)
	if got := transfers(t, sim, block(t, sim, 1).Hash); len(got) != 0 {
		t.Errorf("rejected transactions reached the mempool: %+v", got)
	}
}

func TestTransfersOfUnknownBlock(t *testing.T) {
	sim, _ := newSim(t)
	mine(t, sim, 1)

	_, err := sim.Transfers(context.Background(), "0x"+strings.Repeat("ab", 32))
	if !errors.Is(err, chain.ErrBlockNotFound) {
		t.Errorf("Transfers(unknown hash) error = %v, want ErrBlockNotFound", err)
	}
	// Пустой существующий блок — не ошибка.
	if got, err := sim.Transfers(context.Background(), block(t, sim, 1).Hash); err != nil || len(got) != 0 {
		t.Errorf("Transfers(empty block) = %v, %v; want no transfers and no error", got, err)
	}
}

// Реорг без потери транзакций: выброшенные блоки исчезают, транзакция возвращается в мемпул
// и попадает в следующий блок с тем же номером, но другим хэшем.
func TestReorgReturnsTransactionsToMempool(t *testing.T) {
	sim, _ := newSim(t)
	b1 := mine(t, sim, 1)
	txHash := submit(t, sim, spec(t, alice, "5"))
	mine(t, sim, 2) // блок 2 с транзакцией, блок 3 пустой
	oldBlock := block(t, sim, 2)

	got, err := sim.Reorg(context.Background(), 2, false)
	if err != nil || got != b1 {
		t.Fatalf("Reorg = %+v, %v; want the new head %+v", got, err, b1)
	}
	assertChain(t, sim, 1)
	if _, err := sim.Transfers(context.Background(), oldBlock.Hash); !errors.Is(err, chain.ErrBlockNotFound) {
		t.Errorf("Transfers(dropped block) error = %v, want ErrBlockNotFound", err)
	}

	mine(t, sim, 1)
	assertChain(t, sim, 2)
	newBlock := block(t, sim, 2)
	if newBlock.Hash == oldBlock.Hash || newBlock.ParentHash != b1.Hash {
		t.Errorf("block 2 after the reorg = %+v, want a new hash %s and parent %s", newBlock, oldBlock.Hash, b1.Hash)
	}
	want := chain.Transfer{TxHash: txHash, LogIndex: 0, Token: usdt, From: alice, To: alice, Amount: amt(t, "5")}
	if got := transfers(t, sim, newBlock.Hash); len(got) != 1 || got[0] != want {
		t.Errorf("block 2 after the reorg has %+v, want the same transfer %+v", got, want)
	}
}

// Реорг с потерей транзакций: они исчезают совсем. Транзакция из мемпула реорг не задевает.
func TestReorgDropsTransfers(t *testing.T) {
	sim, _ := newSim(t)
	mine(t, sim, 1)
	submit(t, sim, spec(t, alice, "5"))
	mine(t, sim, 2)
	pending := submit(t, sim, spec(t, bob, "3")) // ещё в мемпуле

	if _, err := sim.Reorg(context.Background(), 2, true); err != nil {
		t.Fatal(err)
	}
	assertChain(t, sim, 1)

	mine(t, sim, 1)
	got := transfers(t, sim, block(t, sim, 2).Hash)
	if len(got) != 1 || got[0].TxHash != pending || got[0].To != bob {
		t.Errorf("block 2 after the reorg has %+v, want only the mempool transaction %s", got, pending)
	}
	mine(t, sim, 3)
	for number := int64(3); number <= 5; number++ {
		if got := transfers(t, sim, block(t, sim, number).Hash); len(got) != 0 {
			t.Errorf("block %d has %+v: the dropped transaction came back", number, got)
		}
	}
}

func TestReorgValidation(t *testing.T) {
	sim, _ := newSim(t)
	ctx := context.Background()
	mine(t, sim, 3)
	submit(t, sim, spec(t, alice, "1"))

	for _, depth := range []int{0, -1} {
		if _, err := sim.Reorg(ctx, depth, false); !errors.Is(err, chain.ErrInvalidDepth) {
			t.Errorf("Reorg(%d) error = %v, want ErrInvalidDepth", depth, err)
		}
	}
	// Глубже цепочки — ошибка, цепочка и мемпул не тронуты.
	for _, drop := range []bool{false, true} {
		if _, err := sim.Reorg(ctx, 4, drop); !errors.Is(err, chain.ErrReorgTooDeep) {
			t.Errorf("Reorg(4, drop=%v) on a chain of 3 blocks: error = %v, want ErrReorgTooDeep", drop, err)
		}
	}
	assertChain(t, sim, 3)
	mine(t, sim, 1)
	if got := transfers(t, sim, block(t, sim, 4).Hash); len(got) != 1 {
		t.Errorf("the mempool transaction was lost by rejected reorgs: %+v", got)
	}

	// Ровно на всю длину — можно: цепочка становится пустой, голова нулевая.
	empty, err := sim.Reorg(ctx, 4, false)
	if err != nil || empty != (chain.Block{}) {
		t.Fatalf("Reorg(4) = %+v, %v; want an empty head", empty, err)
	}
	assertChain(t, sim, 0)
	mine(t, sim, 1)
	assertChain(t, sim, 1)
}

// Параллельные Mine выстраиваются в очередь: номера без дыр и повторов, цепочка не ломается.
func TestConcurrentMine(t *testing.T) {
	sim, pool := newSim(t)
	pgtest.WarmUp(t, pool)

	const miners, perMiner = 10, 3
	errs := make(chan error, miners)
	var wg sync.WaitGroup
	for range miners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sim.Mine(context.Background(), perMiner)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Mine: %v", err)
		}
	}
	assertChain(t, sim, miners*perMiner)
}

// Транзакция не делится между блоками, даже если её отправляют, пока другие добывают блоки.
func TestConcurrentSubmitAndMine(t *testing.T) {
	sim, pool := newSim(t)
	pgtest.WarmUp(t, pool)
	ctx := context.Background()

	const workers = 12
	specs := []chain.TransferSpec{spec(t, alice, "1"), spec(t, bob, "2"), spec(t, alice, "3")}
	txs := make(chan string, workers)
	errs := make(chan error, 2*workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := sim.SubmitTx(ctx, specs)
			errs <- err
			txs <- hash
			_, err = sim.Mine(ctx, 1)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	close(txs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mine(t, sim, 1) // забирает то, что не успело попасть в блоки

	blockOf := map[string]int64{} // tx_hash -> номер блока
	events := map[string]int{}    // tx_hash -> число событий
	for number := int64(1); number <= head(t, sim); number++ {
		for _, tr := range transfers(t, sim, block(t, sim, number).Hash) {
			if prev, ok := blockOf[tr.TxHash]; ok && prev != number {
				t.Errorf("transaction %s is split between blocks %d and %d", tr.TxHash, prev, number)
			}
			blockOf[tr.TxHash] = number
			events[tr.TxHash]++
		}
	}
	for hash := range txs {
		if events[hash] != 3 {
			t.Errorf("transaction %s has %d events in blocks, want 3", hash, events[hash])
		}
	}
}
