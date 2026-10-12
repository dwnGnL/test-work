package scanner

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/chain"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой scanner.

// lockKey — ключ advisory-блокировки «сканер один». Блокировки сканера и симулятора живут
// в одной базе, поэтому ключи у них разные (у симулятора 4001).
const lockKey = 4002

// querier — то, что нужно чтению: под него подходят и соединение (*pgxpool.Conn), и пул
// (*pgxpool.Pool), и транзакция (pgx.Tx).
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// tryLock пробует взять блокировку сессии, не дожидаясь. false — её держит другой экземпляр
// сканера. Блокировка принадлежит соединению q: снимается через unlock на нём же или сама,
// когда соединение закрыто или оборвалось.
func tryLock(ctx context.Context, q querier) (bool, error) {
	var acquired bool
	if err := q.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(lockKey)).Scan(&acquired); err != nil {
		return false, fmt.Errorf("scanner: try lock: %w", err)
	}
	return acquired, nil
}

// unlock снимает блокировку, взятую tryLock на том же соединении q. Если блокировки у соединения
// уже нет, это ErrLockLost.
func unlock(ctx context.Context, q querier) error {
	var released bool
	if err := q.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, int64(lockKey)).Scan(&released); err != nil {
		return fmt.Errorf("scanner: unlock: %w", err)
	}
	if !released {
		return ErrLockLost
	}
	return nil
}

// selectLastBlock возвращает последний обработанный блок (курсор). found = false, если блоков нет.
func selectLastBlock(ctx context.Context, q querier) (b chain.Block, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT number, hash, parent_hash FROM scanner.blocks ORDER BY number DESC LIMIT 1`,
	).Scan(&b.Number, &b.Hash, &b.ParentHash)
	return scanBlock(b, err)
}

// selectBlockBefore возвращает обработанный блок с наибольшим номером меньше number.
func selectBlockBefore(ctx context.Context, q querier, number int64) (b chain.Block, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT number, hash, parent_hash FROM scanner.blocks
		WHERE number < $1 ORDER BY number DESC LIMIT 1`, number,
	).Scan(&b.Number, &b.Hash, &b.ParentHash)
	return scanBlock(b, err)
}

// scanBlock превращает результат Scan в ответ вида «блок, найден ли, ошибка».
func scanBlock(b chain.Block, err error) (chain.Block, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return chain.Block{}, false, nil
	}
	if err != nil {
		return chain.Block{}, false, fmt.Errorf("scanner: select block: %w", err)
	}
	return b, true, nil
}

// insertBlock записывает обработанный блок. Блок записывается один раз: повтор — ошибка
// первичного ключа, а не тихий пропуск, потому что второй проход по тому же блоку значил бы,
// что блокировка «сканер один» не работает.
func insertBlock(ctx context.Context, tx pgx.Tx, b chain.Block) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO scanner.blocks (number, hash, parent_hash) VALUES ($1, $2, $3)`,
		b.Number, b.Hash, b.ParentHash)
	if err != nil {
		return fmt.Errorf("scanner: insert block %d: %w", b.Number, err)
	}
	return nil
}

// deleteBlocksFrom удаляет свои записи о блоках с номером >= fromBlock и возвращает их число.
func deleteBlocksFrom(ctx context.Context, tx pgx.Tx, fromBlock int64) (int, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM scanner.blocks WHERE number >= $1`, fromBlock)
	if err != nil {
		return 0, fmt.Errorf("scanner: delete blocks from %d: %w", fromBlock, err)
	}
	return int(tag.RowsAffected()), nil
}
