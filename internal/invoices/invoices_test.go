package invoices_test

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).
// Тесты проверки до базы базы не требуют, но проходят и с ней.

const (
	hour = time.Hour
	usdt = money.Amount(1_000_000) // один USDT в минимальных единицах
)

var addressRe = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

// params — типичные параметры: 100 USDT, ставка 1,5%, срок час.
func params(partnerID int64, key string) invoices.CreateParams {
	return invoices.CreateParams{PartnerID: partnerID, IdempotencyKey: key, Amount: 100 * usdt, FeeBps: 150, TTL: hour}
}

func countInvoices(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices.invoices`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// same сравнивает счета; время сравниваем по моменту, а не по представлению.
func same(a, b invoices.Invoice) bool {
	return a.ID == b.ID && a.PartnerID == b.PartnerID && a.Amount == b.Amount && a.FeeBps == b.FeeBps &&
		a.DepositAddress == b.DepositAddress && a.ExpiresAt.Equal(b.ExpiresAt) && a.CreatedAt.Equal(b.CreatedAt)
}

// noQueries — Querier, который не должен вызываться: проверки идут до базы.
type noQueries struct{ t *testing.T }

func (q noQueries) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Error("the database was queried")
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("unexpected query") }

func TestCreateNewInvoice(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	inv, created, err := invoices.Create(ctx, pool, params(7, "order-1"))
	if err != nil || !created {
		t.Fatalf("Create = %v, %v; want created", created, err)
	}
	if inv.ID == "" || inv.PartnerID != 7 || inv.Amount != 100*usdt || inv.FeeBps != 150 {
		t.Errorf("invoice = %+v", inv)
	}
	if !addressRe.MatchString(inv.DepositAddress) {
		t.Errorf("deposit address %q is not 0x + 40 lowercase hex digits", inv.DepositAddress)
	}
	if inv.ExpiresAt.Sub(inv.CreatedAt) != hour {
		t.Errorf("expires_at - created_at = %s, want exactly the ttl %s", inv.ExpiresAt.Sub(inv.CreatedAt), hour)
	}
	if time.Since(inv.CreatedAt) > time.Minute || time.Until(inv.ExpiresAt) < 59*time.Minute {
		t.Errorf("times look wrong: created %s, expires %s", inv.CreatedAt, inv.ExpiresAt)
	}

	got, err := invoices.Get(ctx, pool, 7, inv.ID)
	if err != nil || !same(got, inv) {
		t.Errorf("Get = %+v, %v; want %+v", got, err, inv)
	}
}

// Повтор с тем же ключом и тем же запросом возвращает тот же счёт и ничего не пишет.
func TestCreateRepeatReturnsSameInvoice(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	first, created, err := invoices.Create(ctx, pool, params(7, "order-1"))
	if err != nil || !created {
		t.Fatalf("first Create = %v, %v", created, err)
	}

	// Повтор не зависит от ставки и TTL: их задаёт наша сторона, а не запрос партнёра.
	repeat := params(7, "order-1")
	repeat.FeeBps = 999
	repeat.TTL = 5 * hour
	for range 2 {
		again, created, err := invoices.Create(ctx, pool, repeat)
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Error("repeat reported created = true")
		}
		if !same(again, first) {
			t.Errorf("repeat returned %+v, want the original %+v", again, first)
		}
	}
	if n := countInvoices(t, pool); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}
}

// Тот же ключ с другой суммой — ошибка, исходный счёт не меняется.
func TestCreateSameKeyDifferentAmount(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	first, _, err := invoices.Create(ctx, pool, params(7, "order-1"))
	if err != nil {
		t.Fatal(err)
	}
	other := params(7, "order-1")
	other.Amount = 100*usdt + 1
	if _, created, err := invoices.Create(ctx, pool, other); !errors.Is(err, invoices.ErrIdempotencyConflict) || created {
		t.Fatalf("Create = created %v, err %v; want ErrIdempotencyConflict", created, err)
	}

	stored, err := invoices.Get(ctx, pool, 7, first.ID)
	if err != nil || !same(stored, first) {
		t.Errorf("stored invoice changed: %+v, %v", stored, err)
	}
	if n := countInvoices(t, pool); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}
}

// Ключ уникален в паре с партнёром: у другого партнёра тот же ключ — другой счёт.
func TestCreateSameKeyDifferentPartners(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	a, createdA, errA := invoices.Create(ctx, pool, params(1, "order-1"))
	b, createdB, errB := invoices.Create(ctx, pool, params(2, "order-1"))
	if errA != nil || errB != nil || !createdA || !createdB {
		t.Fatalf("Create = (%v, %v), (%v, %v); want both created", createdA, errA, createdB, errB)
	}
	if a.ID == b.ID || a.DepositAddress == b.DepositAddress {
		t.Errorf("partners share an invoice: %+v and %+v", a, b)
	}
	if a.PartnerID != 1 || b.PartnerID != 2 {
		t.Errorf("partner ids = %d, %d", a.PartnerID, b.PartnerID)
	}
	if n := countInvoices(t, pool); n != 2 {
		t.Errorf("invoices in the table = %d, want 2", n)
	}
}

// 20 параллельных Create с одним ключом: одна строка, все получили один и тот же счёт,
// и ровно один вызов создал его.
func TestCreateConcurrentSameKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const workers = 20
	type result struct {
		inv     invoices.Invoice
		created bool
		err     error
	}
	results := make([]result, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			inv, created, err := invoices.Create(ctx, pool, params(7, "order-1"))
			results[i] = result{inv, created, err}
		}()
	}
	close(start)
	wg.Wait()

	createdCount := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d: %v", i, r.err)
		}
		if r.created {
			createdCount++
		}
		if !same(r.inv, results[0].inv) {
			t.Errorf("worker %d got %+v, worker 0 got %+v", i, r.inv, results[0].inv)
		}
	}
	if createdCount != 1 {
		t.Errorf("%d workers reported created = true, want exactly 1", createdCount)
	}
	if n := countInvoices(t, pool); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}
}

// Гонка с разными суммами под одним ключом: побеждает один запрос, остальные с другой суммой
// получают конфликт, а с такой же — исходный счёт.
func TestCreateConcurrentSameKeyDifferentAmounts(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const workers = 20
	amounts := [2]money.Amount{10 * usdt, 20 * usdt}
	type result struct {
		inv     invoices.Invoice
		created bool
		err     error
	}
	results := make([]result, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := params(7, "order-1")
			p.Amount = amounts[i%2]
			<-start
			inv, created, err := invoices.Create(ctx, pool, p)
			results[i] = result{inv, created, err}
		}()
	}
	close(start)
	wg.Wait()

	var winner invoices.Invoice
	createdCount := 0
	for _, r := range results {
		if r.created {
			createdCount++
			winner = r.inv
		}
	}
	if createdCount != 1 {
		t.Fatalf("%d workers reported created = true, want exactly 1", createdCount)
	}
	for i, r := range results {
		switch {
		case amounts[i%2] == winner.Amount && (r.err != nil || !same(r.inv, winner)):
			t.Errorf("worker %d (same amount as the winner): %+v, %v; want the winner's invoice", i, r.inv, r.err)
		case amounts[i%2] != winner.Amount && !errors.Is(r.err, invoices.ErrIdempotencyConflict):
			t.Errorf("worker %d (other amount): err = %v, want ErrIdempotencyConflict", i, r.err)
		}
	}
	if n := countInvoices(t, pool); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}
}

// Create пишет в транзакцию вызывающего: откат убирает счёт, ключ снова свободен.
func TestCreateInCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	sentinel := errors.New("boom")
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		inv, created, err := invoices.Create(ctx, tx, params(7, "order-1"))
		if err != nil || !created {
			t.Errorf("Create in tx = %v, %v", created, err)
		}
		// Внутри транзакции счёт виден.
		if _, err := invoices.Get(ctx, tx, 7, inv.ID); err != nil {
			t.Errorf("Get in tx: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if n := countInvoices(t, pool); n != 0 {
		t.Errorf("invoices after rollback = %d, want 0", n)
	}
	if _, created, err := invoices.Create(ctx, pool, params(7, "order-1")); err != nil || !created {
		t.Errorf("Create after rollback = %v, %v; want created", created, err)
	}
}

// Чужой, несуществующий и записанный не как UUID id — одна и та же ErrNotFound.
func TestGetIsolation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	mine, _, err := invoices.Create(ctx, pool, params(1, "order-1"))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := invoices.Get(ctx, pool, 1, mine.ID); err != nil || !same(got, mine) {
		t.Fatalf("owner: Get = %+v, %v", got, err)
	}
	// Тот же id у другого партнёра не находится.
	if _, err := invoices.Get(ctx, pool, 2, mine.ID); !errors.Is(err, invoices.ErrNotFound) {
		t.Errorf("foreign invoice: err = %v, want ErrNotFound", err)
	}
	// Несуществующий UUID.
	if _, err := invoices.Get(ctx, pool, 1, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, invoices.ErrNotFound) {
		t.Errorf("unknown uuid: err = %v, want ErrNotFound", err)
	}
	// Верхний регистр — та же запись UUID.
	if got, err := invoices.Get(ctx, pool, 1, strings.ToUpper(mine.ID)); err != nil || !same(got, mine) {
		t.Errorf("upper-case id: Get = %+v, %v", got, err)
	}
}

// id не в формате UUID отвечает ErrNotFound, не обращаясь к базе (иначе Postgres вернул бы
// ошибку приведения типа и ответ отличался бы от ответа на чужой счёт).
func TestGetMalformedID(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{
		"",
		"abc",
		"1",
		"11111111-1111-1111-1111-11111111111",   // на символ короче
		"11111111-1111-1111-1111-1111111111111", // на символ длиннее
		"11111111111111111111111111111111",      // без дефисов
		"{11111111-1111-1111-1111-111111111111}",
		"1111111g-1111-1111-1111-111111111111",
		"11111111-1111-1111-1111-111111111111' OR '1'='1",
		"11111111_1111_1111_1111_111111111111",
		"11111111-1111-1111-1111-11111111111я",
	} {
		if _, err := invoices.Get(ctx, noQueries{t}, 1, id); !errors.Is(err, invoices.ErrNotFound) {
			t.Errorf("Get(%q): err = %v, want ErrNotFound", id, err)
		}
	}
}

// Адрес — 0x и 40 hex-символов в нижнем регистре, у каждого счёта свой.
func TestDepositAddresses(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	seen := make(map[string]bool)
	for i := range 50 {
		inv, _, err := invoices.Create(ctx, pool, params(7, "order-"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		if !addressRe.MatchString(inv.DepositAddress) {
			t.Fatalf("address %q is not 0x + 40 lowercase hex digits", inv.DepositAddress)
		}
		if seen[inv.DepositAddress] {
			t.Fatalf("address %s issued twice", inv.DepositAddress)
		}
		seen[inv.DepositAddress] = true
	}
}

// Невалидные параметры отклоняются до базы.
func TestCreateValidatesBeforeDatabase(t *testing.T) {
	ctx := context.Background()

	with := func(f func(*invoices.CreateParams)) invoices.CreateParams {
		p := params(7, "order-1")
		f(&p)
		return p
	}
	tests := []struct {
		name string
		p    invoices.CreateParams
		want error
	}{
		{"partner id zero", with(func(p *invoices.CreateParams) { p.PartnerID = 0 }), invoices.ErrInvalidPartnerID},
		{"partner id negative", with(func(p *invoices.CreateParams) { p.PartnerID = -1 }), invoices.ErrInvalidPartnerID},
		{"empty key", with(func(p *invoices.CreateParams) { p.IdempotencyKey = "" }), invoices.ErrInvalidIdempotencyKey},
		{"key of 256 characters", with(func(p *invoices.CreateParams) { p.IdempotencyKey = strings.Repeat("k", 256) }), invoices.ErrInvalidIdempotencyKey},
		{"key of 256 multibyte characters", with(func(p *invoices.CreateParams) { p.IdempotencyKey = strings.Repeat("я", 256) }), invoices.ErrInvalidIdempotencyKey},
		{"key is not utf-8", with(func(p *invoices.CreateParams) { p.IdempotencyKey = "bad\xff\xfekey" }), invoices.ErrInvalidIdempotencyKey},
		{"amount zero", with(func(p *invoices.CreateParams) { p.Amount = 0 }), invoices.ErrNonPositiveAmount},
		{"amount negative", with(func(p *invoices.CreateParams) { p.Amount = -1 }), invoices.ErrNonPositiveAmount},
		{"fee negative", with(func(p *invoices.CreateParams) { p.FeeBps = -1 }), invoices.ErrInvalidFeeBps},
		{"fee above 100%", with(func(p *invoices.CreateParams) { p.FeeBps = 10_001 }), invoices.ErrInvalidFeeBps},
		{"ttl zero", with(func(p *invoices.CreateParams) { p.TTL = 0 }), invoices.ErrNonPositiveTTL},
		{"ttl negative", with(func(p *invoices.CreateParams) { p.TTL = -time.Second }), invoices.ErrNonPositiveTTL},
		{"ttl below a microsecond", with(func(p *invoices.CreateParams) { p.TTL = 999 * time.Nanosecond }), invoices.ErrNonPositiveTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, created, err := invoices.Create(ctx, noQueries{t}, tt.p)
			if !errors.Is(err, tt.want) || created {
				t.Errorf("Create = created %v, err %v; want %v", created, err, tt.want)
			}
		})
	}
}

// Границы допустимого принимаются: ключ из 255 символов (считаются символы, не байты),
// минимальная сумма, ставки 0 и 10000.
func TestCreateAcceptsBoundaryValues(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	tests := []struct {
		name string
		p    invoices.CreateParams
	}{
		{"key of 255 characters", params(7, strings.Repeat("k", 255))},
		{"key of 255 multibyte characters", params(7, strings.Repeat("я", 255))},
		{"smallest amount", func() invoices.CreateParams { p := params(7, "min"); p.Amount = 1; return p }()},
		{"fee 0", func() invoices.CreateParams { p := params(7, "fee0"); p.FeeBps = 0; return p }()},
		{"fee 10000", func() invoices.CreateParams { p := params(7, "fee100"); p.FeeBps = 10_000; return p }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, created, err := invoices.Create(ctx, pool, tt.p)
			if err != nil || !created {
				t.Fatalf("Create = %v, %v", created, err)
			}
			if inv.Amount != tt.p.Amount || inv.FeeBps != tt.p.FeeBps {
				t.Errorf("invoice = %+v, want amount %s and fee %d", inv, tt.p.Amount, tt.p.FeeBps)
			}
		})
	}
}
