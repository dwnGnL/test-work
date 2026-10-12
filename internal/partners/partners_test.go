package partners_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/partners"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

// create создаёт партнёра в собственной транзакции базы и фиксирует её.
func create(ctx context.Context, pool *pgxpool.Pool, name, apiKey string, feeBps int) (p partners.Partner, err error) {
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		p, err = partners.Create(ctx, tx, name, apiKey, feeBps)
		return err
	})
	return p, err
}

func countPartners(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM partners.partners`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countPartnerAccounts(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger.accounts WHERE code LIKE 'partner:%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Партнёр создаётся вместе со счётом в ledger: баланс ноль, а не ErrUnknownAccount.
func TestCreateMakesLedgerAccount(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	p, err := create(ctx, pool, "shop", "key-shop", 150)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Name != "shop" || p.FeeBps != 150 {
		t.Errorf("partner = %+v, want id set, name shop, fee 150", p)
	}

	bal, err := ledger.PartnerBalance(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("PartnerBalance: %v (the ledger account must exist right after Create)", err)
	}
	if bal != 0 {
		t.Errorf("balance = %s, want 0", bal)
	}
}

// Откат транзакции убирает и партнёра, и его счёт: частичного состояния не остаётся.
func TestCreateRollsBackWithCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	sentinel := errors.New("boom")
	var id int64
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		p, err := partners.Create(ctx, tx, "shop", "key-shop", 150)
		if err != nil {
			return err
		}
		id = p.ID
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the sentinel", err)
	}

	if n := countPartners(t, pool); n != 0 {
		t.Errorf("partners = %d, want 0 after rollback", n)
	}
	if _, err := ledger.PartnerBalance(ctx, pool, id); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("PartnerBalance after rollback: err = %v, want ErrUnknownAccount", err)
	}

	// Ключ после отката свободен.
	if _, err := create(ctx, pool, "shop", "key-shop", 150); err != nil {
		t.Errorf("create after rollback: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	tests := []struct {
		name    string
		pname   string
		apiKey  string
		feeBps  int
		wantErr error
	}{
		{"empty name", "", "k", 100, partners.ErrEmptyName},
		{"blank name", "   ", "k", 100, partners.ErrEmptyName},
		{"empty key", "shop", "", 100, partners.ErrEmptyAPIKey},
		{"key with leading space", "shop", " k", 100, partners.ErrInvalidAPIKey},
		{"key with trailing space", "shop", "k ", 100, partners.ErrInvalidAPIKey},
		{"key with space inside", "shop", "a b", 100, partners.ErrInvalidAPIKey},
		{"key with tab", "shop", "a\tb", 100, partners.ErrInvalidAPIKey},
		{"key with newline", "shop", "a\nb", 100, partners.ErrInvalidAPIKey},
		{"negative fee", "shop", "k", -1, partners.ErrInvalidFeeBps},
		{"fee above 100%", "shop", "k", 10_001, partners.ErrInvalidFeeBps},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := create(ctx, pool, tt.pname, tt.apiKey, tt.feeBps); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if n := countPartners(t, pool); n != 0 {
		t.Errorf("partners = %d, want 0: invalid parameters must not reach the table", n)
	}
	if n := countPartnerAccounts(t, pool); n != 0 {
		t.Errorf("partner accounts = %d, want 0", n)
	}

	// Границы допустимого диапазона принимаются.
	for _, edge := range []struct {
		key string
		bps int
	}{{"edge-key-0", 0}, {"edge-key-max", 10_000}} {
		if _, err := create(ctx, pool, "edge", edge.key, edge.bps); err != nil {
			t.Errorf("fee %d: %v", edge.bps, err)
		}
	}
}

// Занятый ключ — ErrAPIKeyTaken, второго партнёра и второго счёта нет.
func TestCreateDuplicateKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	if _, err := create(ctx, pool, "first", "same-key", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := create(ctx, pool, "second", "same-key", 200); !errors.Is(err, partners.ErrAPIKeyTaken) {
		t.Fatalf("err = %v, want ErrAPIKeyTaken", err)
	}
	if n := countPartners(t, pool); n != 1 {
		t.Errorf("partners = %d, want 1", n)
	}
	if n := countPartnerAccounts(t, pool); n != 1 {
		t.Errorf("partner accounts = %d, want 1", n)
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	alpha, err := create(ctx, pool, "alpha", "key-alpha", 150)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := create(ctx, pool, "beta", "key-beta", 50)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid keys", func(t *testing.T) {
		for _, want := range []struct {
			key string
			p   partners.Partner
		}{{"key-alpha", alpha}, {"key-beta", beta}} {
			got, err := partners.Authenticate(ctx, pool, want.key)
			if err != nil {
				t.Fatalf("Authenticate(%q): %v", want.key, err)
			}
			if got != want.p {
				t.Errorf("Authenticate(%q) = %+v, want %+v", want.key, got, want.p)
			}
		}
	})

	t.Run("works on a transaction", func(t *testing.T) {
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			got, err := partners.Authenticate(ctx, tx, "key-alpha")
			if err == nil && got != alpha {
				t.Errorf("got %+v, want %+v", got, alpha)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong keys", func(t *testing.T) {
		for _, key := range []string{"nope", "key-alph", "KEY-ALPHA", "key-alpha ", " key-alpha", "key-alpha\x00"} {
			if _, err := partners.Authenticate(ctx, pool, key); !errors.Is(err, partners.ErrUnauthorized) {
				t.Errorf("Authenticate(%q): err = %v, want ErrUnauthorized", key, err)
			}
		}
	})

	// Пустой ключ отклоняется до базы: запрос к ней был бы ошибкой.
	t.Run("empty key does not touch the database", func(t *testing.T) {
		_, err := partners.Authenticate(ctx, noQueries{t}, "")
		if !errors.Is(err, partners.ErrUnauthorized) {
			t.Errorf("err = %v, want ErrUnauthorized", err)
		}
	})
}

// noQueries — Querier, который не должен вызываться.
type noQueries struct{ t *testing.T }

func (q noQueries) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Error("the database was queried")
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("unexpected query") }

// В базе лежит только sha256 ключа, самого ключа нет ни в одной колонке.
func TestAPIKeyIsStoredHashed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	const key = "super-secret-api-key"
	if _, err := create(ctx, pool, "shop", key, 100); err != nil {
		t.Fatal(err)
	}

	var hash []byte
	if err := pool.QueryRow(ctx, `SELECT api_key_hash FROM partners.partners`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if string(hash) != string(sum[:]) {
		t.Errorf("api_key_hash = %x, want sha256 of the key %x", hash, sum)
	}

	// Вся строка целиком, в текстовом и в json-виде: ключа в ней нет.
	var row string
	if err := pool.QueryRow(ctx, `SELECT p::text || row_to_json(p)::text FROM partners.partners p`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, key) {
		t.Errorf("the plain key is visible in the stored row: %s", row)
	}
}

// SeedDemo создаёт ровно двух партнёров со счетами; повтор и параллельные вызовы ничего не добавляют.
func TestSeedDemo(t *testing.T) {
	ctx := context.Background()

	assertDemo := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		if n := countPartners(t, pool); n != 2 {
			t.Errorf("partners = %d, want 2", n)
		}
		if n := countPartnerAccounts(t, pool); n != 2 {
			t.Errorf("partner accounts = %d, want 2", n)
		}
		for key, wantBps := range map[string]int{"demo-key-alpha": 150, "demo-key-beta": 50} {
			p, err := partners.Authenticate(ctx, pool, key)
			if err != nil {
				t.Fatalf("Authenticate(%q): %v", key, err)
			}
			if p.FeeBps != wantBps {
				t.Errorf("%s: fee = %d, want %d", p.Name, p.FeeBps, wantBps)
			}
			if bal, err := ledger.PartnerBalance(ctx, pool, p.ID); err != nil || bal != 0 {
				t.Errorf("%s: balance = %v, err = %v; want 0", p.Name, bal, err)
			}
		}
	}

	t.Run("twice in a row", func(t *testing.T) {
		pool := pgtest.NewMigrated(t)
		for range 2 {
			if err := partners.SeedDemo(ctx, pool); err != nil {
				t.Fatal(err)
			}
		}
		assertDemo(t, pool)
	})

	t.Run("in parallel", func(t *testing.T) {
		pool := pgtest.NewMigrated(t)
		pgtest.WarmUp(t, pool)

		const workers = 10
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = partners.SeedDemo(ctx, pool)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}
		assertDemo(t, pool)
	})

	// База, где демо-партнёры уже созданы старой миграцией 0008 (том docker compose
	// живёт между запусками): повторно их не создаём и ничего не ломаем.
	t.Run("partners left by the old migration", func(t *testing.T) {
		pool := pgtest.NewMigrated(t)
		_, err := pool.Exec(ctx, `
			INSERT INTO partners.partners (name, api_key_hash, fee_bps) VALUES
			  ('alpha', sha256('demo-key-alpha'::bytea), 150),
			  ('beta',  sha256('demo-key-beta'::bytea),  50);
			INSERT INTO ledger.accounts (code)
			SELECT 'partner:' || id FROM partners.partners ORDER BY id;`)
		if err != nil {
			t.Fatal(err)
		}
		if err := partners.SeedDemo(ctx, pool); err != nil {
			t.Fatal(err)
		}
		assertDemo(t, pool)
	})
}
