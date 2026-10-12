package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/money"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой payments.

// paymentColumns — колонки в порядке полей Payment, как их читает RowToStructByPos.
const paymentColumns = `id, invoice_id::text, partner_id, tx_hash, log_index, from_address, amount, fee_bps,
	block_number, block_hash, status`

// Querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// upsertDetected записывает перевод одним запросом. Ключ — (tx_hash, log_index), блок — изменяемый
// атрибут. Нет платежа — вставляется detected. Есть в detected или orphaned — получает новый блок
// и статус detected (из orphaned транзакция включена заново). Есть в aml_pending, credited,
// rejected — WHERE не пускает обновление, строка остаётся как была.
func upsertDetected(ctx context.Context, tx pgx.Tx, p DetectParams) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payments.payments AS p
			(invoice_id, partner_id, tx_hash, log_index, from_address, amount, fee_bps,
			 block_number, block_hash, status)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, 'detected')
		ON CONFLICT (tx_hash, log_index) DO UPDATE
			SET block_number = EXCLUDED.block_number,
			    block_hash   = EXCLUDED.block_hash,
			    status       = 'detected',
			    updated_at   = now()
			WHERE p.status IN ('detected', 'orphaned')`,
		p.InvoiceID, p.PartnerID, p.TxHash, p.LogIndex, p.From, int64(p.Amount), p.FeeBps,
		p.BlockNumber, p.BlockHash)
	if err != nil {
		return fmt.Errorf("payments: detect %s/%d: %w", p.TxHash, p.LogIndex, err)
	}
	return nil
}

// orphanDetected переводит платежи detected из блоков с номером >= fromBlock в orphaned
// и обнуляет их блок. Возвращает, сколько платежей переведено.
func orphanDetected(ctx context.Context, tx pgx.Tx, fromBlock int64) (int, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE payments.payments
		SET status = 'orphaned', block_number = NULL, block_hash = NULL, updated_at = now()
		WHERE status = 'detected' AND block_number >= $1`, fromBlock)
	if err != nil {
		return 0, fmt.Errorf("payments: orphan payments from block %d: %w", fromBlock, err)
	}
	return int(tag.RowsAffected()), nil
}

// countPastThreshold считает платежи, уже прошедшие порог (aml_pending и дальше) в блоках
// с номером >= fromBlock. Их реорг не обрабатывается, они только считаются.
func countPastThreshold(ctx context.Context, tx pgx.Tx, fromBlock int64) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM payments.payments
		WHERE status IN ('aml_pending', 'credited', 'rejected') AND block_number >= $1`,
		fromBlock).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("payments: count confirmed payments from block %d: %w", fromBlock, err)
	}
	return n, nil
}

// selectReady возвращает id платежей detected, чей блок не выше maxBlock, то есть набравших порог.
func selectReady(ctx context.Context, q Querier, maxBlock int64) ([]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT id FROM payments.payments
		WHERE status = 'detected' AND block_number <= $1
		ORDER BY id`, maxBlock)
	if err != nil {
		return nil, fmt.Errorf("payments: select ready payments: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("payments: select ready payments: %w", err)
	}
	return ids, nil
}

// markAMLPending — условная смена статуса detected → aml_pending. Порог проверяется ещё раз
// в самом запросе: между выборкой и обновлением платёж могли откатить или перенести в другой блок.
// moved = false значит, что строка не изменилась: платёж уже обработан другим вызовом или
// больше не готов. Возвращает сумму платежа для проводки.
func markAMLPending(ctx context.Context, tx pgx.Tx, id, maxBlock int64) (amount money.Amount, moved bool, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE payments.payments
		SET status = 'aml_pending', updated_at = now()
		WHERE id = $1 AND status = 'detected' AND block_number <= $2
		RETURNING amount`, id, maxBlock).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("payments: mark payment %d as aml_pending: %w", id, err)
	}
	return amount, true, nil
}

// claimForAML захватывает до limit платежей aml_pending, чья очередь проверки пришла: платёж ещё
// ни разу не проверялся или его пауза истекла (next_attempt_at <= now). Новый платёж берётся без
// оглядки на время: его next_attempt_at выставлен часами базы, а now приходит от приложения, и при
// расхождении часов он ждал бы зря. Всё одним запросом: увеличивает aml_attempts и сдвигает next_attempt_at
// на паузу, которая растёт с числом прошлых попыток: first, 2·first, 4·first ... не больше maxPause.
// Дальше это «аренда» платежа на время паузы:
//   - FOR UPDATE SKIP LOCKED: параллельный захват берёт другие платежи и не ждёт чужих блокировок;
//   - захваченный платёж другой воркер не возьмёт, пока пауза не истечёт;
//   - AML недоступен или воркер упал — платёж сам вернётся в очередь по истечении паузы,
//     отдельной записи о неудаче не нужно.
//
// Степень двойки ограничена: иначе при очень долгой недоступности AML (сотни попыток) power()
// переполнила бы double precision, и захват падал бы целиком для всех платежей.
func claimForAML(ctx context.Context, q Querier, now time.Time, limit int, first, maxPause time.Duration) ([]Payment, error) {
	rows, err := q.Query(ctx, `
		UPDATE payments.payments
		SET aml_attempts    = aml_attempts + 1,
		    next_attempt_at = $1::timestamptz
		                      + make_interval(secs => LEAST($2::float8 * power(2, LEAST(aml_attempts, 20)), $3::float8)),
		    updated_at      = now()
		WHERE id IN (
			SELECT id FROM payments.payments
			WHERE status = 'aml_pending' AND (aml_attempts = 0 OR next_attempt_at <= $1::timestamptz)
			ORDER BY next_attempt_at, id
			LIMIT $4
			FOR UPDATE SKIP LOCKED)
		RETURNING `+paymentColumns, now, first.Seconds(), maxPause.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("payments: claim payments for AML: %w", err)
	}
	claimed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Payment])
	if err != nil {
		return nil, fmt.Errorf("payments: claim payments for AML: %w", err)
	}
	return claimed, nil
}

// markDecided — условная смена статуса aml_pending → credited | rejected вместе с вердиктом и
// временем проверки. Статус и вердикт меняются одним оператором: CHECK базы пропускает credited
// только с low и rejected только с high. decided = false значит, что строка не изменилась:
// платёж уже решён другим воркером, и вызывающий выходит без проводок.
func markDecided(ctx context.Context, tx pgx.Tx, id int64, status Status, verdict aml.Verdict, checkedAt time.Time) (decided bool, err error) {
	tag, err := tx.Exec(ctx, `
		UPDATE payments.payments
		SET status = $2, aml_verdict = $3, aml_checked_at = $4, updated_at = now()
		WHERE id = $1 AND status = 'aml_pending'`, id, string(status), string(verdict), checkedAt)
	if err != nil {
		return false, fmt.Errorf("payments: mark payment %d as %s: %w", id, status, err)
	}
	return tag.RowsAffected() == 1, nil
}

// selectSummary считает по счёту сумму платежей credited (поле amount, до комиссии) и
// признак «есть платежи в обработке» (detected или aml_pending). Агрегат без группировки
// всегда возвращает одну строку, у счёта без платежей это ноль и false.
func selectSummary(ctx context.Context, q Querier, invoiceID string) (received money.Amount, inProgress bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE status = 'credited'), 0)::bigint,
		       COALESCE(bool_or(status IN ('detected', 'aml_pending')), false)
		FROM payments.payments WHERE invoice_id = $1::uuid`, invoiceID).Scan(&received, &inProgress)
	if err != nil {
		return 0, false, fmt.Errorf("payments: summary of invoice %s: %w", invoiceID, err)
	}
	return received, inProgress, nil
}

// selectByInvoice читает все платежи счёта в порядке создания.
func selectByInvoice(ctx context.Context, q Querier, invoiceID string) ([]Payment, error) {
	rows, err := q.Query(ctx, `
		SELECT `+paymentColumns+`
		FROM payments.payments WHERE invoice_id = $1::uuid ORDER BY id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("payments: list payments of invoice %s: %w", invoiceID, err)
	}
	payments, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Payment])
	if err != nil {
		return nil, fmt.Errorf("payments: list payments of invoice %s: %w", invoiceID, err)
	}
	return payments, nil
}
