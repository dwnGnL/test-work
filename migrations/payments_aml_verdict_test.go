package migrations_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
	"github.com/dwnGnL/test-work/migrations"
)

// Интеграционный тест миграции 0010: зачисление и отказ только с вердиктом AML. Нужен TEST_DATABASE_URL.
func TestPaymentsAMLVerdict(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if err := postgres.Migrate(ctx, pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	const checkViolation = "23514"
	low, high, medium := "low", "high", "medium"

	// insert вставляет платёж с заданными статусом и вердиктом (nil — NULL).
	insert := func(txHash, status string, verdict *string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO payments.payments (invoice_id, partner_id, tx_hash, log_index, from_address, amount, fee_bps, status, aml_verdict)
			VALUES ('11111111-1111-1111-1111-111111111111', 1, $1, 0, '0xfrom', 5, 150, $2, $3)`,
			txHash, status, verdict)
		return err
	}

	t.Run("insert", func(t *testing.T) {
		tests := []struct {
			name    string
			status  string
			verdict *string
			code    string
		}{
			{"credited without verdict", "credited", nil, checkViolation},
			{"credited with high risk", "credited", &high, checkViolation},
			{"credited with low risk", "credited", &low, ""},
			{"rejected without verdict", "rejected", nil, checkViolation},
			{"rejected with low risk", "rejected", &low, checkViolation},
			{"rejected with high risk", "rejected", &high, ""},
			{"detected without verdict", "detected", nil, ""},
			{"aml_pending without verdict", "aml_pending", nil, ""},
			{"orphaned without verdict", "orphaned", nil, ""},
			{"unknown verdict", "aml_pending", &medium, checkViolation},
		}
		for i, tt := range tests {
			err := insert("0xinsert"+itoa(int64(i)), tt.status, tt.verdict)
			if got := pgCode(err); got != tt.code {
				t.Errorf("%s: code = %q (err = %v), want %q", tt.name, got, err, tt.code)
			}
		}
	})

	// Код переводит платёж в новый статус UPDATE-ом: ограничения должны держать и этот путь.
	t.Run("update", func(t *testing.T) {
		tests := []struct {
			name    string
			status  string
			verdict *string
			code    string
		}{
			{"to credited without verdict", "credited", nil, checkViolation},
			{"to credited with high risk", "credited", &high, checkViolation},
			{"to credited with low risk", "credited", &low, ""},
			{"to rejected without verdict", "rejected", nil, checkViolation},
			{"to rejected with low risk", "rejected", &low, checkViolation},
			{"to rejected with high risk", "rejected", &high, ""},
		}
		for i, tt := range tests {
			hash := "0xupdate" + itoa(int64(i))
			if err := insert(hash, "aml_pending", nil); err != nil {
				t.Fatalf("%s: seed payment: %v", tt.name, err)
			}
			_, err := pool.Exec(ctx,
				`UPDATE payments.payments SET status = $2, aml_verdict = $3, aml_checked_at = now() WHERE tx_hash = $1`,
				hash, tt.status, tt.verdict)
			if got := pgCode(err); got != tt.code {
				t.Errorf("%s: code = %q (err = %v), want %q", tt.name, got, err, tt.code)
			}
		}

		// Вердикт можно убрать только вместе с выходом из credited/rejected: статус без вердикта не проходит.
		if err := insert("0xupdate-clear", "credited", &low); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `UPDATE payments.payments SET aml_verdict = NULL WHERE tx_hash = '0xupdate-clear'`)
		if got := pgCode(err); got != checkViolation {
			t.Errorf("clearing the verdict of a credited payment: code = %q (err = %v), want %q", got, err, checkViolation)
		}
	})
}
