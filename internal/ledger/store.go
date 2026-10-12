package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/money"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой ledger.

// Querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertAccount создаёт счёт, если его ещё нет.
func insertAccount(ctx context.Context, tx pgx.Tx, code string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO ledger.accounts (code) VALUES ($1) ON CONFLICT (code) DO NOTHING`, code)
	if err != nil {
		return fmt.Errorf("ledger: ensure account %q: %w", code, err)
	}
	return nil
}

// accountIDsByCode возвращает id счетов в порядке строк проводки. Все счета ищутся одним
// запросом; отсутствующий счёт — ErrUnknownAccount.
func accountIDsByCode(ctx context.Context, tx pgx.Tx, lines []Line) ([]int64, error) {
	codes := make([]string, len(lines))
	for i, l := range lines {
		codes[i] = l.Account
	}

	rows, err := tx.Query(ctx, `SELECT code, id FROM ledger.accounts WHERE code = ANY($1)`, codes)
	if err != nil {
		return nil, fmt.Errorf("ledger: look up accounts: %w", err)
	}
	defer rows.Close()

	byCode := make(map[string]int64, len(codes))
	for rows.Next() {
		var code string
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			return nil, fmt.Errorf("ledger: look up accounts: %w", err)
		}
		byCode[code] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: look up accounts: %w", err)
	}

	ids := make([]int64, len(codes))
	for i, code := range codes {
		id, ok := byCode[code]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownAccount, code)
		}
		ids[i] = id
	}
	return ids, nil
}

// insertTransaction вставляет заголовок проводки. inserted = false значит, что ключ
// идемпотентности уже занят и ничего не вставлено.
func insertTransaction(ctx context.Context, tx pgx.Tx, key, kind string) (id int64, inserted bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ($1, $2)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, key, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ledger: insert transaction %q: %w", key, err)
	}
	return id, true, nil
}

// insertEntries записывает все строки проводки одним запросом. В базу уходят два массива,
// номера счетов и суммы со знаком, а unnest разворачивает их в строки. Запрос один,
// поэтому записаны либо все строки, либо ни одной.
func insertEntries(ctx context.Context, tx pgx.Tx, txID int64, accountIDs []int64, entries []entry) error {
	amounts := make([]int64, len(entries))
	for i, e := range entries {
		amounts[i] = e.Amount
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO ledger.entries (transaction_id, account_id, amount)
		SELECT $1::bigint, u.account_id, u.amount
		FROM unnest($2::bigint[], $3::bigint[]) AS u(account_id, amount)`,
		txID, accountIDs, amounts)
	if err != nil {
		return fmt.Errorf("ledger: insert entries: %w", err)
	}
	return nil
}

// loadStored читает проводку, уже записанную под ключом: её вид и строки.
func loadStored(ctx context.Context, tx pgx.Tx, key string) (kind string, entries []entry, err error) {
	var txID int64
	err = tx.QueryRow(ctx, `SELECT id, kind FROM ledger.transactions WHERE idempotency_key = $1`,
		key).Scan(&txID, &kind)
	if err != nil {
		// Вставка только что упёрлась в этот заголовок, он обязан быть виден.
		return "", nil, fmt.Errorf("ledger: load transaction %q: %w", key, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT a.code, e.amount
		FROM ledger.entries e JOIN ledger.accounts a ON a.id = e.account_id
		WHERE e.transaction_id = $1`, txID)
	if err != nil {
		return "", nil, fmt.Errorf("ledger: load entries of %q: %w", key, err)
	}
	entries, err = pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil {
		return "", nil, fmt.Errorf("ledger: load entries of %q: %w", key, err)
	}
	return kind, entries, nil
}

// selectBalance возвращает сумму проводок счёта со знаком. found = false, если счёта нет.
func selectBalance(ctx context.Context, q Querier, code string) (sum money.Amount, found bool, err error) {
	// GROUP BY нужен, чтобы у несуществующего счёта не было ни одной строки:
	// агрегат без группировки вернул бы одну строку с нулём.
	err = q.QueryRow(ctx, `
		SELECT COALESCE(sum(e.amount), 0)::bigint
		FROM ledger.accounts a LEFT JOIN ledger.entries e ON e.account_id = a.id
		WHERE a.code = $1
		GROUP BY a.id`, code).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ledger: balance of %q: %w", code, err)
	}
	return sum, true, nil
}

// selectTotals возвращает сальдо по всем счетам, включая счета без проводок.
func selectTotals(ctx context.Context, q Querier) (map[string]money.Amount, error) {
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

// selectEntriesSum возвращает сумму всех проводок журнала.
func selectEntriesSum(ctx context.Context, q Querier) (money.Amount, error) {
	var sum money.Amount
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM ledger.entries`).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("ledger: sum of entries: %w", err)
	}
	return sum, nil
}
