package scanner_test

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/partners"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/internal/scanner"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).
// Время не используется: тесты вызывают Tick напрямую и ничего не ждут.

const (
	// usdtConfig — адрес контракта в том виде, в каком его пишут в конфигурации (с контрольной суммой);
	// usdt — тот же адрес в нижнем регистре, как его отдаёт сеть.
	usdtConfig    = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	usdt          = "0xdac17f958d2ee523a2206206994597c13d831ec7"
	fakeToken     = "0x00000000000000000000000000000000000000f4"
	sender        = "0x1111111111111111111111111111111111111111"
	stranger      = "0x2222222222222222222222222222222222222222"
	confirmations = 3
)

// safeBuffer — буфер лога, безопасный для записи из нескольких горутин.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fixture — база с симулятором сети и сканером (порог — три подтверждения).
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	sim  *chain.Simulator
	sc   *scanner.Scanner
	logs *safeBuffer
	seq  int // счётчик для уникальных ключей партнёров и счетов
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewMigrated(t)
	f := &fixture{t: t, pool: pool, sim: chain.NewSimulator(pool), logs: &safeBuffer{}}
	f.sc = f.newScanner(f.sim)
	return f
}

// newScanner создаёт ещё один сканер на той же базе (второй экземпляр или рестарт).
func (f *fixture) newScanner(network chain.Chain) *scanner.Scanner {
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return scanner.New(f.pool, network, confirmations, usdtConfig, log)
}

// newInvoice создаёт партнёра с комиссией 1,5% и его счёт на 100 USDT.
func (f *fixture) newInvoice() invoices.Invoice {
	f.t.Helper()
	ctx := context.Background()
	f.seq++
	n := strconv.Itoa(f.seq)

	var p partners.Partner
	err := postgres.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		var err error
		p, err = partners.Create(ctx, tx, "partner-"+n, "key-"+n, 150)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	inv, _, err := invoices.Create(ctx, f.pool, invoices.CreateParams{
		PartnerID: p.ID, IdempotencyKey: "order-" + n, Amount: amt(f.t, "100"), FeeBps: p.FeeBps, TTL: time.Hour,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return inv
}

func amt(t testing.TB, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// usdtTo — перевод USDT на адрес to.
func usdtTo(t testing.TB, to, amount string) chain.TransferSpec {
	t.Helper()
	return chain.TransferSpec{Token: usdt, From: sender, To: to, Amount: amt(t, amount)}
}

// send отправляет одну транзакцию с переводами и возвращает её хэш.
func (f *fixture) send(specs ...chain.TransferSpec) string {
	f.t.Helper()
	hash, err := f.sim.SubmitTx(context.Background(), specs)
	if err != nil {
		f.t.Fatalf("SubmitTx: %v", err)
	}
	return hash
}

func (f *fixture) mine(count int) chain.Block {
	f.t.Helper()
	head, err := f.sim.Mine(context.Background(), count)
	if err != nil {
		f.t.Fatalf("Mine(%d): %v", count, err)
	}
	return head
}

func (f *fixture) reorg(depth int, drop bool) {
	f.t.Helper()
	if _, err := f.sim.Reorg(context.Background(), depth, drop); err != nil {
		f.t.Fatalf("Reorg(%d, %v): %v", depth, drop, err)
	}
}

func (f *fixture) tick() {
	f.t.Helper()
	if err := f.sc.Tick(context.Background()); err != nil {
		f.t.Fatalf("Tick: %v", err)
	}
}

// payments возвращает платежи счёта.
func (f *fixture) payments(inv invoices.Invoice) []payments.Payment {
	f.t.Helper()
	list, err := payments.ListByInvoice(context.Background(), f.pool, inv.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

// onlyPayment требует, чтобы у счёта был ровно один платёж, и возвращает его.
func (f *fixture) onlyPayment(inv invoices.Invoice) payments.Payment {
	f.t.Helper()
	list := f.payments(inv)
	if len(list) != 1 {
		f.t.Fatalf("payments of the invoice = %+v, want exactly one", list)
	}
	return list[0]
}

// assertHold сверяет холд: на депозитах ровно want, в холде -want, остальные счета пусты,
// журнал сходится. Платёж после порога даёт одну проводку на свою сумму, поэтому сумма
// сальдо однозначно говорит, сколько платежей проведено.
func (f *fixture) assertHold(want money.Amount) {
	f.t.Helper()
	ctx := context.Background()
	totals, err := ledger.Totals(ctx, f.pool)
	if err != nil {
		f.t.Fatal(err)
	}
	if totals[ledger.AccountDeposits] != want || totals[ledger.AccountHoldAML] != -want {
		f.t.Errorf("assets:deposits = %s, hold:aml = %s; want %s and %s",
			totals[ledger.AccountDeposits], totals[ledger.AccountHoldAML], want, -want)
	}
	for _, code := range []string{ledger.AccountRefundsPayable, ledger.AccountRevenueFees} {
		if totals[code] != 0 {
			f.t.Errorf("%s = %s, want 0", code, totals[code])
		}
	}
	if err := ledger.CheckBalanced(ctx, f.pool); err != nil {
		f.t.Errorf("CheckBalanced: %v", err)
	}
}

// scannedBlocks читает таблицу обработанных блоков сканера.
func (f *fixture) scannedBlocks() []chain.Block {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT number, hash, parent_hash FROM scanner.blocks ORDER BY number`)
	if err != nil {
		f.t.Fatal(err)
	}
	blocks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[chain.Block])
	if err != nil {
		f.t.Fatal(err)
	}
	return blocks
}

// assertCursor проверяет, что сканер обработал блоки 1..head подряд, а хэши и parent_hash совпадают с сетью.
func (f *fixture) assertCursor(head int64) {
	f.t.Helper()
	got := f.scannedBlocks()
	if int64(len(got)) != head {
		f.t.Fatalf("scanner has %d blocks, want %d", len(got), head)
	}
	for i, b := range got {
		number := int64(i + 1)
		want, found, err := f.sim.Block(context.Background(), number)
		if err != nil || !found {
			f.t.Fatalf("network block %d: %+v, %v, %v", number, want, found, err)
		}
		if b != want {
			f.t.Errorf("scanner block %d = %+v, network has %+v", number, b, want)
		}
	}
}

func (f *fixture) logContains(s string) bool { return strings.Contains(f.logs.String(), s) }

// hookChain оборачивает сеть и даёт тесту вмешаться в чтение: подменить события блока или
// переписать цепочку в момент, когда сканер читает её.
type hookChain struct {
	chain.Chain
	onHead      func()
	onBlock     func(number int64)
	onTransfers func(got []chain.Transfer) []chain.Transfer
}

func (h *hookChain) Head(ctx context.Context) (int64, error) {
	if h.onHead != nil {
		h.onHead()
	}
	return h.Chain.Head(ctx)
}

func (h *hookChain) Block(ctx context.Context, number int64) (chain.Block, bool, error) {
	if h.onBlock != nil {
		h.onBlock(number)
	}
	return h.Chain.Block(ctx, number)
}

func (h *hookChain) Transfers(ctx context.Context, blockHash string) ([]chain.Transfer, error) {
	got, err := h.Chain.Transfers(ctx, blockHash)
	if err != nil || h.onTransfers == nil {
		return got, err
	}
	return h.onTransfers(got), nil
}
