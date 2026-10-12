package partners

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой partners.

// Querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertPartner вставляет партнёра. inserted = false значит, что такой ключ (его хэш) уже занят
// и ничего не вставлено. При гонке за один ключ проигравший ждёт коммита победителя.
func insertPartner(ctx context.Context, tx pgx.Tx, name string, keyHash []byte, feeBps int) (p Partner, inserted bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO partners.partners (name, api_key_hash, fee_bps) VALUES ($1, $2, $3)
		ON CONFLICT (api_key_hash) DO NOTHING
		RETURNING id, name, fee_bps`, name, keyHash, feeBps).Scan(&p.ID, &p.Name, &p.FeeBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return Partner{}, false, nil
	}
	if err != nil {
		return Partner{}, false, fmt.Errorf("partners: insert %q: %w", name, err)
	}
	return p, true, nil
}

// selectByKeyHash ищет партнёра по хэшу ключа. found = false, если такого нет.
func selectByKeyHash(ctx context.Context, q Querier, keyHash []byte) (p Partner, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT id, name, fee_bps FROM partners.partners WHERE api_key_hash = $1`,
		keyHash).Scan(&p.ID, &p.Name, &p.FeeBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return Partner{}, false, nil
	}
	if err != nil {
		return Partner{}, false, fmt.Errorf("partners: look up by key: %w", err)
	}
	return p, true, nil
}
