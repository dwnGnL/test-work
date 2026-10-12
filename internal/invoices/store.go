package invoices

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой invoices.

// invoiceColumns — колонки в порядке полей Invoice, как их читают scanInvoice и selectByKey.
const invoiceColumns = `id::text, partner_id, amount, fee_bps, deposit_address, expires_at, created_at`

// Querier — то, что нужно запросам: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertInvoice вставляет счёт. inserted = false значит, что ключ идемпотентности партнёра
// уже занят и ничего не вставлено. При гонке за один ключ проигравший ждёт коммита победителя.
// Срок жизни считает база: created_at и expires_at берутся из одних часов и различаются ровно на TTL.
func insertInvoice(ctx context.Context, q Querier, p CreateParams, requestHash []byte, depositAddress string) (inv Invoice, inserted bool, err error) {
	row := q.QueryRow(ctx, `
		INSERT INTO invoices.invoices
			(partner_id, idempotency_key, request_hash, amount, fee_bps, deposit_address, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + $7::bigint * interval '1 microsecond')
		ON CONFLICT (partner_id, idempotency_key) DO NOTHING
		RETURNING `+invoiceColumns,
		p.PartnerID, p.IdempotencyKey, requestHash, int64(p.Amount), p.FeeBps, depositAddress, p.TTL.Microseconds())
	inv, err = scanInvoice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, false, nil
	}
	if err != nil {
		return Invoice{}, false, fmt.Errorf("invoices: insert: %w", err)
	}
	return inv, true, nil
}

// selectByKey читает счёт партнёра по ключу идемпотентности вместе с хэшем запроса,
// под которым он был создан. found = false, если такого счёта нет.
func selectByKey(ctx context.Context, q Querier, partnerID int64, key string) (inv Invoice, requestHash []byte, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT `+invoiceColumns+`, request_hash
		FROM invoices.invoices
		WHERE partner_id = $1 AND idempotency_key = $2`, partnerID, key).Scan(
		&inv.ID, &inv.PartnerID, &inv.Amount, &inv.FeeBps, &inv.DepositAddress,
		&inv.ExpiresAt, &inv.CreatedAt, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, nil, false, nil
	}
	if err != nil {
		return Invoice{}, nil, false, fmt.Errorf("invoices: look up by idempotency key: %w", err)
	}
	return inv, requestHash, true, nil
}

// selectByID читает счёт по id и партнёру. Фильтр по партнёру здесь же, в запросе: чужой счёт
// просто не находится. found = false, если такого счёта у этого партнёра нет.
func selectByID(ctx context.Context, q Querier, partnerID int64, id string) (inv Invoice, found bool, err error) {
	inv, err = scanInvoice(q.QueryRow(ctx, `
		SELECT `+invoiceColumns+`
		FROM invoices.invoices
		WHERE id = $1::uuid AND partner_id = $2`, id, partnerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, false, nil
	}
	if err != nil {
		return Invoice{}, false, fmt.Errorf("invoices: look up by id: %w", err)
	}
	return inv, true, nil
}

// selectByDepositAddress читает счёт по депозитному адресу. Фильтра по партнёру нет: адрес
// уникален в таблице, а искать по нему будет сканер блоков, которому партнёр не известен.
// found = false, если такого счёта нет.
func selectByDepositAddress(ctx context.Context, q Querier, address string) (inv Invoice, found bool, err error) {
	inv, err = scanInvoice(q.QueryRow(ctx, `
		SELECT `+invoiceColumns+`
		FROM invoices.invoices
		WHERE deposit_address = $1`, address))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, false, nil
	}
	if err != nil {
		return Invoice{}, false, fmt.Errorf("invoices: look up by deposit address: %w", err)
	}
	return inv, true, nil
}

// scanInvoice читает строку с колонками invoiceColumns.
func scanInvoice(row pgx.Row) (Invoice, error) {
	var inv Invoice
	err := row.Scan(&inv.ID, &inv.PartnerID, &inv.Amount, &inv.FeeBps, &inv.DepositAddress,
		&inv.ExpiresAt, &inv.CreatedAt)
	return inv, err
}
