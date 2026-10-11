package migrations_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

var moduleSchemas = []string{"partners", "invoices", "chainsim", "scanner", "payments", "ledger", "refunds"}

// Правило модульного монолита: SQL модуля обращается только к его собственной схеме.
// Единственное исключение — демо-сид 0008, он явно помечен в самом файле.
func TestMigrationsTouchOnlyOwnSchema(t *testing.T) {
	own := map[string][]string{
		"0001_partners.sql":                   {"partners"},
		"0002_invoices.sql":                   {"invoices"},
		"0003_chainsim.sql":                   {"chainsim"},
		"0004_scanner.sql":                    {"scanner"},
		"0005_payments.sql":                   {"payments"},
		"0006_ledger.sql":                     {"ledger"},
		"0007_refunds.sql":                    {"refunds"},
		"0008_seed_demo.sql":                  {"partners", "ledger"}, // демо-данные: partner:{id} подставляется подзапросом
		"0009_ledger_hardening.sql":           {"ledger"},
		"0010_payments_aml_verdict.sql":       {"payments"},
		"0011_invoices_address_lowercase.sql": {"invoices"},
		"0012_payments_address_lowercase.sql": {"payments"},
		"0013_refunds_address_lowercase.sql":  {"refunds"},
		"0014_chainsim_address_lowercase.sql": {"chainsim"},
	}
	refRe := regexp.MustCompile(`\b(` + strings.Join(moduleSchemas, "|") + `)\.[a-z_]`)

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(own) {
		t.Errorf("found %d migration files, test knows %d: update the table", len(entries), len(own))
	}
	for _, e := range entries {
		allowed, ok := own[e.Name()]
		if !ok {
			t.Errorf("unknown migration file %s", e.Name())
			continue
		}
		body, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range refRe.FindAllStringSubmatch(stripComments(string(body)), -1) {
			if !contains(allowed, m[1]) {
				t.Errorf("%s refers to foreign schema %q", e.Name(), m[1])
			}
		}
	}
}

func stripComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, l := range lines {
		if idx := strings.Index(l, "--"); idx >= 0 {
			lines[i] = l[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Интеграционный тест: ограничения и триггеры схемы. Нужен TEST_DATABASE_URL.
func TestSchema(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if err := postgres.Migrate(ctx, pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	t.Run("no foreign keys between schemas", func(t *testing.T) {
		rows, err := pool.Query(ctx, `
			SELECT c.conrelid::regclass::text, c.confrelid::regclass::text
			FROM pg_constraint c
			JOIN pg_class src ON src.oid = c.conrelid
			JOIN pg_class dst ON dst.oid = c.confrelid
			WHERE c.contype = 'f' AND src.relnamespace <> dst.relnamespace`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var from, to string
			if err := rows.Scan(&from, &to); err != nil {
				t.Fatal(err)
			}
			t.Errorf("foreign key across schemas: %s -> %s", from, to)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("demo seed", func(t *testing.T) {
		want := map[string]struct {
			bps int
			key string
		}{"alpha": {150, "demo-key-alpha"}, "beta": {50, "demo-key-beta"}}

		rows, err := pool.Query(ctx, `SELECT id, name, fee_bps, api_key_hash FROM partners.partners`)
		if err != nil {
			t.Fatal(err)
		}
		type partner struct {
			id   int64
			name string
		}
		var got []partner
		for rows.Next() {
			var (
				p    partner
				bps  int
				hash []byte
			)
			if err := rows.Scan(&p.id, &p.name, &bps, &hash); err != nil {
				t.Fatal(err)
			}
			w, ok := want[p.name]
			if !ok {
				t.Fatalf("unexpected partner %q", p.name)
			}
			sum := sha256.Sum256([]byte(w.key))
			if bps != w.bps || string(hash) != string(sum[:]) {
				t.Errorf("partner %s: fee_bps=%d (want %d), hash matches=%t", p.name, bps, w.bps, string(hash) == string(sum[:]))
			}
			got = append(got, p)
		}
		rows.Close()
		if len(got) != 2 {
			t.Fatalf("got %d partners, want 2", len(got))
		}
		for _, p := range got {
			var n int
			code := "partner:" + itoa(p.id)
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger.accounts WHERE code = $1`, code).Scan(&n); err != nil || n != 1 {
				t.Errorf("ledger account %s: count=%d err=%v", code, n, err)
			}
		}
	})

	t.Run("system ledger accounts", func(t *testing.T) {
		for _, code := range []string{"assets:deposits", "hold:aml", "refunds:payable", "revenue:fees"} {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger.accounts WHERE code = $1`, code).Scan(&n); err != nil || n != 1 {
				t.Errorf("system account %s: count=%d err=%v", code, n, err)
			}
		}
	})

	t.Run("ledger: unbalanced transaction fails on commit", func(t *testing.T) {
		err := post(ctx, pool, "unbalanced:1", 100, -99)
		assertCode(t, err, "23514") // check_violation из отложенного триггера
	})

	t.Run("ledger: single-sided transaction fails on commit", func(t *testing.T) {
		assertCode(t, post(ctx, pool, "unbalanced:2", 100), "23514")
	})

	t.Run("ledger: balanced transaction commits", func(t *testing.T) {
		if err := post(ctx, pool, "balanced:1", 10_000_000, -1_500_000, -8_500_000); err != nil {
			t.Fatalf("balanced transaction rejected: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
			WHERE t.idempotency_key = 'balanced:1'`).Scan(&n); err != nil || n != 3 {
			t.Errorf("entries stored = %d, err = %v", n, err)
		}
	})

	t.Run("ledger: sum can be non-zero inside the transaction", func(t *testing.T) {
		// Проверка отложена до коммита: промежуточная несбалансированность допустима.
		if err := post(ctx, pool, "balanced:2", 5, 5, -10); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ledger: no overflow in balance check", func(t *testing.T) {
		const max = int64(9223372036854775807)
		assertCode(t, post(ctx, pool, "overflow:1", max, max), "23514") // сумма не помещается в bigint, но и не ноль
		if err := post(ctx, pool, "overflow:2", max, -max); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ledger: idempotency key is unique", func(t *testing.T) {
		assertCode(t, post(ctx, pool, "balanced:1", 1, -1), "23505")
	})

	t.Run("ledger: zero entry rejected", func(t *testing.T) {
		assertCode(t, post(ctx, pool, "zero:1", 0, 0), "23514")
	})

	t.Run("ledger: journal is append-only", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE ledger.entries SET amount = amount`,
			`DELETE FROM ledger.entries`,
			`UPDATE ledger.transactions SET kind = kind`,
			`DELETE FROM ledger.transactions`,
		} {
			_, err := pool.Exec(ctx, stmt)
			if code := pgCode(err); code != "23001" {
				t.Errorf("%s: code = %q (err = %v), want 23001", stmt, code, err)
			}
		}
	})

	t.Run("constraints", func(t *testing.T) {
		const invID = "11111111-1111-1111-1111-111111111111"
		tests := []struct {
			name string
			sql  string
			code string
		}{
			{"partner fee above 100%", `INSERT INTO partners.partners (name, api_key_hash, fee_bps) VALUES ('x', '\x01', 10001)`, "23514"},
			{"partner fee negative", `INSERT INTO partners.partners (name, api_key_hash, fee_bps) VALUES ('x', '\x02', -1)`, "23514"},
			{"partner key unique", `INSERT INTO partners.partners (name, api_key_hash, fee_bps) SELECT 'dup', api_key_hash, 0 FROM partners.partners LIMIT 1`, "23505"},
			{"invoice amount zero", invoiceInsert("a", "k1", 0, "0xa1"), "23514"},
			{"invoice fee above 100%", invoiceInsert("a", "k1", 1, "0xa1", 10001), "23514"},
			{"invoice ok", invoiceInsert("a", "k1", 1, "0xa1"), ""},
			{"invoice idempotency key unique per partner", invoiceInsert("a", "k1", 1, "0xa2"), "23505"},
			{"invoice same key other partner ok", invoiceInsert("b", "k1", 1, "0xa3"), ""},
			{"invoice deposit address unique", invoiceInsert("b", "k2", 1, "0xa1"), "23505"},
			{"payment ok", paymentInsert(invID, "0xtx", 0, 5, "detected"), ""},
			{"payment key (tx_hash, log_index) unique", paymentInsert(invID, "0xtx", 0, 5, "detected"), "23505"},
			{"payment same tx other log_index ok", paymentInsert(invID, "0xtx", 1, 5, "detected"), ""},
			{"payment amount zero", paymentInsert(invID, "0xtx2", 0, 0, "detected"), "23514"},
			{"payment bad status", paymentInsert(invID, "0xtx3", 0, 5, "paid"), "23514"},
			{"refund ok", `INSERT INTO refunds.refunds (payment_id, to_address, amount, status) VALUES (1, '0xa', 5, 'created')`, ""},
			{"refund one per payment", `INSERT INTO refunds.refunds (payment_id, to_address, amount, status) VALUES (1, '0xb', 5, 'created')`, "23505"},
			{"refund bad status", `INSERT INTO refunds.refunds (payment_id, to_address, amount, status) VALUES (2, '0xa', 5, 'sent')`, "23514"},
			{"chainsim block ok", `INSERT INTO chainsim.blocks (number, hash, parent_hash) VALUES (1, '0xb1', '0xb0')`, ""},
			{"chainsim transfer in mempool ok", transferInsert("0xt", 0, "NULL", 5), ""},
			{"chainsim transfer unknown block", transferInsert("0xt2", 0, "'0xnope'", 5), "23503"},
			{"chainsim transfer key unique", transferInsert("0xt", 0, "NULL", 5), "23505"},
			{"chainsim transfer negative amount", transferInsert("0xt3", 0, "NULL", -1), "23514"},
			{"scanner block ok", `INSERT INTO scanner.blocks (number, hash, parent_hash) VALUES (1, '0xb1', '0xb0')`, ""},
			{"scanner block number unique", `INSERT INTO scanner.blocks (number, hash, parent_hash) VALUES (1, '0xb2', '0xb0')`, "23505"},
		}
		for _, tt := range tests {
			_, err := pool.Exec(ctx, tt.sql)
			if got := pgCode(err); got != tt.code {
				t.Errorf("%s: code = %q (err = %v), want %q", tt.name, got, err, tt.code)
			}
		}
	})

	t.Run("chainsim: dropping a block returns transfers to mempool", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE chainsim.transfers SET block_hash = '0xb1' WHERE tx_hash = '0xt'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM chainsim.blocks WHERE hash = '0xb1'`); err != nil {
			t.Fatal(err)
		}
		var hash *string
		if err := pool.QueryRow(ctx, `SELECT block_hash FROM chainsim.transfers WHERE tx_hash = '0xt'`).Scan(&hash); err != nil || hash != nil {
			t.Errorf("block_hash = %v, err = %v; want NULL (ON DELETE SET NULL)", hash, err)
		}
	})
}

// post пишет транзакцию ledger с проводками на два счёта (поочерёдно) и возвращает результат коммита.
func post(ctx context.Context, pool *pgxpool.Pool, key string, amounts ...int64) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var txID int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ($1, 'test') RETURNING id`, key).Scan(&txID); err != nil {
			return err
		}
		for i, a := range amounts {
			code := "assets:deposits"
			if i%2 == 1 {
				code = "hold:aml"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger.entries (transaction_id, account_id, amount)
				SELECT $1, id, $2 FROM ledger.accounts WHERE code = $3`, txID, a, code); err != nil {
				return err
			}
		}
		return nil
	})
}

func invoiceInsert(partner, key string, amount int, addr string, bps ...int) string {
	fee := 150
	if len(bps) > 0 {
		fee = bps[0]
	}
	pid := map[string]string{"a": "(SELECT id FROM partners.partners WHERE name = 'alpha')", "b": "(SELECT id FROM partners.partners WHERE name = 'beta')"}[partner]
	return `INSERT INTO invoices.invoices (partner_id, idempotency_key, request_hash, amount, fee_bps, deposit_address, expires_at)
		VALUES (` + pid + `, '` + key + `', '\x00', ` + itoa(int64(amount)) + `, ` + itoa(int64(fee)) + `, '` + addr + `', now() + interval '1 hour')`
}

func paymentInsert(invoice, txHash string, logIndex, amount int, status string) string {
	return `INSERT INTO payments.payments (invoice_id, partner_id, tx_hash, log_index, from_address, amount, fee_bps, status)
		VALUES ('` + invoice + `', 1, '` + txHash + `', ` + itoa(int64(logIndex)) + `, '0xfrom', ` + itoa(int64(amount)) + `, 150, '` + status + `')`
}

func transferInsert(txHash string, logIndex int, blockHash string, amount int) string {
	return `INSERT INTO chainsim.transfers (tx_hash, log_index, token, from_address, to_address, amount, block_hash)
		VALUES ('` + txHash + `', ` + itoa(int64(logIndex)) + `, '0xtoken', '0xfrom', '0xto', ` + itoa(int64(amount)) + `, ` + blockHash + `)`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := pgCode(err); got != want {
		t.Errorf("SQLSTATE = %q (err = %v), want %q", got, err, want)
	}
}
