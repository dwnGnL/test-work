package payments_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

// Интеграционные тесты: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

const (
	invoiceA = "11111111-1111-1111-1111-111111111111"
	invoiceB = "22222222-2222-2222-2222-222222222222"
	sender   = "0x1111111111111111111111111111111111111111"
	fee      = 150 // 1,5% в базисных пунктах
)

func amt(t testing.TB, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// params — перевод 100 USDT на счёт A, замеченный в блоке block. Хэш транзакции задаёт tx.
func params(t testing.TB, tx string, logIndex int, block int64) payments.DetectParams {
	t.Helper()
	return payments.DetectParams{
		InvoiceID: invoiceA, PartnerID: 7, TxHash: "0x" + tx, LogIndex: logIndex, From: sender,
		Amount: amt(t, "100"), FeeBps: fee, BlockNumber: block, BlockHash: blockHash(block),
	}
}

// blockHash — хэш блока с номером n; другой хэш того же номера задают тесты явно.
func blockHash(n int64) string {
	return "0xb" + string(rune('a'+n%26)) + "000000000000000000000000000000000000000000000000000000000000"
}

// detect записывает перевод в собственной транзакции.
func detect(ctx context.Context, pool *pgxpool.Pool, p payments.DetectParams) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		return payments.Detect(ctx, tx, p)
	})
}

func mustDetect(t testing.TB, pool *pgxpool.Pool, p payments.DetectParams) {
	t.Helper()
	if err := detect(context.Background(), pool, p); err != nil {
		t.Fatalf("Detect(%s/%d): %v", p.TxHash, p.LogIndex, err)
	}
}

// row — платёж в виде, удобном для проверок.
type row struct {
	ID          int64
	Status      payments.Status
	BlockNumber *int64
	BlockHash   *string
}

// find читает платёж по ключу (tx_hash, log_index).
func find(t testing.TB, pool *pgxpool.Pool, txHash string, logIndex int) row {
	t.Helper()
	var r row
	err := pool.QueryRow(context.Background(), `
		SELECT id, status, block_number, block_hash FROM payments.payments
		WHERE tx_hash = $1 AND log_index = $2`, txHash, logIndex).Scan(&r.ID, &r.Status, &r.BlockNumber, &r.BlockHash)
	if err != nil {
		t.Fatalf("find %s/%d: %v", txHash, logIndex, err)
	}
	return r
}

func countPayments(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM payments.payments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setStatus ставит платежу статус напрямую, минуя обработчик: тестам чтения нужны платежи во
// всех статусах. Для credited и rejected CHECK базы требует вердикт AML, поэтому он ставится
// вместе со статусом.
func setStatus(t testing.TB, pool *pgxpool.Pool, id int64, status payments.Status) {
	t.Helper()
	verdict := map[payments.Status]*string{payments.StatusCredited: ptr("low"), payments.StatusRejected: ptr("high")}[status]
	_, err := pool.Exec(context.Background(),
		`UPDATE payments.payments SET status = $2, aml_verdict = $3 WHERE id = $1`, id, string(status), verdict)
	if err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

func ptr[T any](v T) *T { return &v }
