package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/money"
)

// Querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Balance возвращает сальдо счёта: сумму его проводок со знаком (дебет > 0, кредит < 0).
// Счёт без проводок даёт 0, несуществующий счёт — ErrUnknownAccount.
func Balance(ctx context.Context, q Querier, code string) (money.Amount, error) {
	var sum money.Amount
	// GROUP BY нужен, чтобы у несуществующего счёта не было ни одной строки:
	// агрегат без группировки вернул бы одну строку с нулём.
	err := q.QueryRow(ctx, `
		SELECT COALESCE(sum(e.amount), 0)::bigint
		FROM ledger.accounts a LEFT JOIN ledger.entries e ON e.account_id = a.id
		WHERE a.code = $1
		GROUP BY a.id`, code).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %q", ErrUnknownAccount, code)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: balance of %q: %w", code, err)
	}
	return sum, nil
}

// PartnerBalance возвращает баланс партнёра как положительное число.
//
// Счёт партнёра — обязательство сервиса перед ним: зачисление идёт кредитом (проводка < 0),
// поэтому сальдо счёта отрицательное. Партнёру показываем то же число с обратным знаком:
// положительное, если сервис ему должен.
func PartnerBalance(ctx context.Context, q Querier, partnerID int64) (money.Amount, error) {
	sum, err := Balance(ctx, q, PartnerAccount(partnerID))
	if err != nil {
		return 0, err
	}
	return -sum, nil
}

// Totals возвращает сальдо по всем счетам (со знаком, как Balance), включая счета без проводок.
// Сумма по всем счетам обязана быть нулём: это проверка для тестов и сверки. Читает всю
// таблицу проводок, в обработчиках запросов её вызывать не нужно.
func Totals(ctx context.Context, q Querier) (map[string]money.Amount, error) {
	rows, err := q.Query(ctx, `
		SELECT a.code, COALESCE(sum(e.amount), 0)::bigint
		FROM ledger.accounts a LEFT JOIN ledger.entries e ON e.account_id = a.id
		GROUP BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("ledger: totals: %w", err)
	}
	defer rows.Close()

	totals := make(map[string]money.Amount)
	for rows.Next() {
		var code string
		var sum money.Amount
		if err := rows.Scan(&code, &sum); err != nil {
			return nil, fmt.Errorf("ledger: totals: %w", err)
		}
		totals[code] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: totals: %w", err)
	}
	return totals, nil
}
