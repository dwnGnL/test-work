package ledger_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).
// Тесты проверки до базы (TestPostRejectsInvalid...) базы не требуют.

// newDB создаёт пустую базу с применёнными миграциями.
func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	if err := postgres.Migrate(context.Background(), pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// warmUp заранее открывает все соединения пула. Иначе горутины теста получают соединения
// по одному, по мере их открытия, и выполняются почти по очереди, а гонки не возникает.
func warmUp(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var conns []*pgxpool.Conn
	for range pool.Config().MaxConns {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

// amt разбирает сумму вида "98.5".
func amt(t testing.TB, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// post проводит p в собственной транзакции базы и фиксирует её.
func post(ctx context.Context, pool *pgxpool.Pool, p ledger.Posting) (posted bool, err error) {
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		posted, err = ledger.Post(ctx, tx, p)
		return err
	})
	return posted, err
}

// mustPost проводит p и требует, чтобы проводка была записана впервые.
func mustPost(t *testing.T, pool *pgxpool.Pool, p ledger.Posting) {
	t.Helper()
	posted, err := post(context.Background(), pool, p)
	if err != nil || !posted {
		t.Fatalf("Post(%s) = %v, %v; want true, nil", p.Key, posted, err)
	}
}

// ensure создаёт счёт в собственной транзакции базы.
func ensure(ctx context.Context, pool *pgxpool.Pool, code string) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		return ledger.EnsureAccount(ctx, tx, code)
	})
}

// count выполняет запрос вида SELECT count(*) ...
func count(t testing.TB, q ledger.Querier, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// journalSize возвращает число заголовков и число проводок в журнале.
func journalSize(t testing.TB, q ledger.Querier) (transactions, entries int) {
	t.Helper()
	return count(t, q, `SELECT count(*) FROM ledger.transactions`),
		count(t, q, `SELECT count(*) FROM ledger.entries`)
}

// assertTotals сверяет сальдо перечисленных счетов и проверяет главный инвариант:
// сумма по всем счетам — ноль. Остальные счета на ожидаемые значения не проверяются,
// но в сумму входят.
func assertTotals(t *testing.T, q ledger.Querier, want map[string]money.Amount) {
	t.Helper()
	got, err := ledger.Totals(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	for code, w := range want {
		if g, ok := got[code]; !ok || g != w {
			t.Errorf("Totals[%s] = %v (present: %v), want %v", code, g, ok, w)
		}
	}

	var sum money.Amount
	for _, v := range got {
		sum += v
	}
	if sum != 0 {
		t.Errorf("sum over all accounts = %s, want 0", sum)
	}
}
