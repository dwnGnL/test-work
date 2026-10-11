package ledger

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Системные счета. Они создаются миграцией 0006, поэтому EnsureAccount для них не нужен.
const (
	AccountDeposits       = "assets:deposits" // USDT на депозитных адресах (актив)
	AccountHoldAML        = "hold:aml"        // платёж ждёт решения AML
	AccountRefundsPayable = "refunds:payable" // долг сервиса перед отправителями (возвраты)
	AccountRevenueFees    = "revenue:fees"    // комиссия сервиса
)

// PartnerAccount возвращает код счёта партнёра: partner:{id}.
// Сам счёт создаёт EnsureAccount; до его создания Balance и Post вернут ErrUnknownAccount.
func PartnerAccount(partnerID int64) string {
	return "partner:" + strconv.FormatInt(partnerID, 10)
}

// EnsureAccount создаёт счёт, если его ещё нет. Повтор и параллельные вызовы безопасны:
// при гонке проигравший ждёт коммита победителя и ничего не делает.
// Пишет в транзакцию вызывающего: если она откатится, счёт тоже исчезнет.
func EnsureAccount(ctx context.Context, tx pgx.Tx, code string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO ledger.accounts (code) VALUES ($1) ON CONFLICT (code) DO NOTHING`, code)
	if err != nil {
		return fmt.Errorf("ledger: ensure account %q: %w", code, err)
	}
	return nil
}
