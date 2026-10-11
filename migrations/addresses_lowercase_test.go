package migrations_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

// Интеграционный тест миграций 0011–0014: адреса хранятся только в нижнем регистре.
// Нужен TEST_DATABASE_URL.
func TestAddressesLowercase(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if err := postgres.Migrate(ctx, pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	const (
		lower = "0xdac17f958d2ee523a2206206994597c13d831ec7"
		eip55 = "0xdAC17F958D2ee523a2206206994597C13D831ec7" // тот же адрес с контрольным регистром
	)
	const checkViolation = "23514"

	invoice := func(key, addr string) string {
		return `INSERT INTO invoices.invoices (partner_id, idempotency_key, request_hash, amount, fee_bps, deposit_address, expires_at)
			VALUES (1, '` + key + `', '\x00', 1, 150, '` + addr + `', now() + interval '1 hour')`
	}
	payment := func(txHash, from string) string {
		return `INSERT INTO payments.payments (invoice_id, partner_id, tx_hash, log_index, from_address, amount, fee_bps, status)
			VALUES ('11111111-1111-1111-1111-111111111111', 1, '` + txHash + `', 0, '` + from + `', 5, 150, 'detected')`
	}
	refund := func(paymentID int, to string) string {
		return `INSERT INTO refunds.refunds (payment_id, to_address, amount, status)
			VALUES (` + itoa(int64(paymentID)) + `, '` + to + `', 5, 'created')`
	}
	transfer := func(txHash, token, from, to string) string {
		return `INSERT INTO chainsim.transfers (tx_hash, log_index, token, from_address, to_address, amount)
			VALUES ('` + txHash + `', 0, '` + token + `', '` + from + `', '` + to + `', 5)`
	}

	tests := []struct {
		name       string
		sql        string
		constraint string // ограничение, которое должно сработать; "" — вставка проходит
	}{
		{"invoice deposit address lowercase ok", invoice("k1", lower), ""},
		{"invoice deposit address EIP-55", invoice("k2", eip55), "invoices_deposit_address_lowercase"},
		// Без CHECK UNIQUE пропустил бы второй счёт с тем же адресом в другом регистре.
		{"invoice same address in other case", invoice("k3", eip55), "invoices_deposit_address_lowercase"},
		{"invoice same address again is unique violation", invoice("k4", lower), "invoices_deposit_address_key"},

		{"payment from_address lowercase ok", payment("0xp1", lower), ""},
		{"payment from_address EIP-55", payment("0xp2", eip55), "payments_from_address_lowercase"},

		{"refund to_address lowercase ok", refund(1, lower), ""},
		{"refund to_address EIP-55", refund(2, eip55), "refunds_to_address_lowercase"},

		{"transfer all lowercase ok", transfer("0xt1", lower, lower, lower), ""},
		{"transfer token EIP-55", transfer("0xt2", eip55, lower, lower), "transfers_token_lowercase"},
		{"transfer from_address EIP-55", transfer("0xt3", lower, eip55, lower), "transfers_from_address_lowercase"},
		{"transfer to_address EIP-55", transfer("0xt4", lower, lower, eip55), "transfers_to_address_lowercase"},
	}
	for _, tt := range tests {
		_, err := pool.Exec(ctx, tt.sql)
		var pgErr *pgconn.PgError
		switch {
		case tt.constraint == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		case tt.constraint == "":
			// вставка прошла, как и ожидалось
		case !errors.As(err, &pgErr):
			t.Errorf("%s: err = %v, want violation of %s", tt.name, err, tt.constraint)
		case pgErr.ConstraintName != tt.constraint:
			t.Errorf("%s: constraint = %q (code %s), want %q", tt.name, pgErr.ConstraintName, pgErr.Code, tt.constraint)
		case tt.constraint != "invoices_deposit_address_key" && pgErr.Code != checkViolation:
			t.Errorf("%s: code = %s, want %s", tt.name, pgErr.Code, checkViolation)
		}
	}

	// Старые строки не мешают: UPDATE адреса в смешанный регистр тоже отклоняется.
	_, err := pool.Exec(ctx, `UPDATE payments.payments SET from_address = $1 WHERE tx_hash = '0xp1'`, eip55)
	assertCode(t, err, checkViolation)
}
