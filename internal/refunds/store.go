package refunds

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой refunds.

// refundColumns — колонки в порядке полей Refund, как их читает RowToStructByPos.
const refundColumns = `id, payment_id, to_address, amount, status`

// Querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Execer — то, что нужно записи одним оператором: пул, соединение и транзакция умеют Exec.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertRefund вставляет возврат со статусом created. На повторе (возврат по платежу уже есть)
// ничего не вставляется, inserted = false: уникальность payment_id держит база. Параллельный
// вставщик ждёт коммита первого и тоже получает inserted = false; если первый откатился,
// вставка проходит у второго.
func insertRefund(ctx context.Context, tx pgx.Tx, p CreateParams) (r Refund, inserted bool, err error) {
	rows, err := tx.Query(ctx, `
		INSERT INTO refunds.refunds (payment_id, to_address, amount, status)
		VALUES ($1, $2, $3, 'created')
		ON CONFLICT (payment_id) DO NOTHING
		RETURNING `+refundColumns, p.PaymentID, p.ToAddress, int64(p.Amount))
	if err != nil {
		return Refund{}, false, fmt.Errorf("refunds: insert refund of payment %d: %w", p.PaymentID, err)
	}
	r, err = pgx.CollectOneRow(rows, pgx.RowToStructByPos[Refund])
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, nil
	}
	if err != nil {
		return Refund{}, false, fmt.Errorf("refunds: insert refund of payment %d: %w", p.PaymentID, err)
	}
	return r, true, nil
}

// selectByPayment читает возврат платежа. Вызывается после конфликта вставки, поэтому строка
// должна быть; её отсутствие — ошибка (pgx.ErrNoRows внутри).
func selectByPayment(ctx context.Context, q Querier, paymentID int64) (Refund, error) {
	rows, err := q.Query(ctx, `SELECT `+refundColumns+` FROM refunds.refunds WHERE payment_id = $1`, paymentID)
	if err != nil {
		return Refund{}, fmt.Errorf("refunds: read refund of payment %d: %w", paymentID, err)
	}
	r, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[Refund])
	if err != nil {
		return Refund{}, fmt.Errorf("refunds: read refund of payment %d: %w", paymentID, err)
	}
	return r, nil
}

// markReady — условная смена статуса created → ready_to_send у всех возвратов в created.
// Повтор и параллельный вызов безопасны: строку, уже переведённую другим вызовом, WHERE
// пропускает. Возвращает, сколько возвратов переведено этим вызовом.
func markReady(ctx context.Context, db Execer) (int, error) {
	tag, err := db.Exec(ctx, `
		UPDATE refunds.refunds SET status = 'ready_to_send', updated_at = now()
		WHERE status = 'created'`)
	if err != nil {
		return 0, fmt.Errorf("refunds: mark refunds as ready_to_send: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// selectByPayments читает возвраты перечисленных платежей одним запросом.
func selectByPayments(ctx context.Context, q Querier, paymentIDs []int64) ([]Refund, error) {
	rows, err := q.Query(ctx, `
		SELECT `+refundColumns+` FROM refunds.refunds WHERE payment_id = ANY($1) ORDER BY id`, paymentIDs)
	if err != nil {
		return nil, fmt.Errorf("refunds: read refunds of payments: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Refund])
	if err != nil {
		return nil, fmt.Errorf("refunds: read refunds of payments: %w", err)
	}
	return list, nil
}
