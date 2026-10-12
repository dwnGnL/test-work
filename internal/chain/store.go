package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Весь SQL модуля лежит в этом файле. Остальные файлы пакета к базе напрямую не обращаются:
// в них проверки и порядок шагов. Модуль работает только со схемой chainsim.

// lockKey — ключ advisory-блокировки симулятора. Блокировки сканера и симулятора живут
// в одной базе, поэтому ключи у них разные (у сканера 4002).
const lockKey = 4001

// querier — то, что нужно чтению: под него подходят и пул (*pgxpool.Pool), и транзакция (pgx.Tx).
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockChain берёт блокировку до конца транзакции. Mine и Reorg меняют цепочку целиком
// (читают голову, дописывают или режут хвост), поэтому идут строго по очереди.
// Следующий запрос после ожидания видит всё, что закоммитил предыдущий владелец (READ COMMITTED).
func lockChain(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(lockKey)); err != nil {
		return fmt.Errorf("chain: lock: %w", err)
	}
	return nil
}

// selectHead возвращает последний блок. found = false, если блоков ещё нет.
func selectHead(ctx context.Context, q querier) (b Block, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT number, hash, parent_hash FROM chainsim.blocks ORDER BY number DESC LIMIT 1`,
	).Scan(&b.Number, &b.Hash, &b.ParentHash)
	return scanBlock(b, err, "head")
}

// selectBlock возвращает блок по номеру. found = false, если такого блока нет.
func selectBlock(ctx context.Context, q querier, number int64) (b Block, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT number, hash, parent_hash FROM chainsim.blocks WHERE number = $1`, number,
	).Scan(&b.Number, &b.Hash, &b.ParentHash)
	return scanBlock(b, err, "block")
}

// scanBlock превращает результат Scan в ответ вида «блок, найден ли, ошибка».
func scanBlock(b Block, err error, what string) (Block, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Block{}, false, nil
	}
	if err != nil {
		return Block{}, false, fmt.Errorf("chain: select %s: %w", what, err)
	}
	return b, true, nil
}

// blockExists проверяет, есть ли блок с таким хэшем.
func blockExists(ctx context.Context, q querier, hash string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chainsim.blocks WHERE hash = $1)`, hash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("chain: check block %s: %w", hash, err)
	}
	return exists, nil
}

// insertBlocks записывает блоки одним запросом: в базу уходят три массива, unnest разворачивает их в строки.
func insertBlocks(ctx context.Context, tx pgx.Tx, blocks []Block) error {
	numbers := make([]int64, len(blocks))
	hashes := make([]string, len(blocks))
	parents := make([]string, len(blocks))
	for i, b := range blocks {
		numbers[i], hashes[i], parents[i] = b.Number, b.Hash, b.ParentHash
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO chainsim.blocks (number, hash, parent_hash)
		SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[])`, numbers, hashes, parents)
	if err != nil {
		return fmt.Errorf("chain: insert blocks: %w", err)
	}
	return nil
}

// insertTransfers кладёт события одной транзакции в мемпул (block_hash IS NULL) одним запросом:
// транзакция либо попадает в мемпул вся, либо не попадает вовсе. log_index — позиция в списке.
func insertTransfers(ctx context.Context, q querier, txHash string, specs []TransferSpec) error {
	logIndexes := make([]int32, len(specs))
	tokens := make([]string, len(specs))
	froms := make([]string, len(specs))
	tos := make([]string, len(specs))
	amounts := make([]int64, len(specs))
	for i, s := range specs {
		logIndexes[i], tokens[i], froms[i], tos[i], amounts[i] = int32(i), s.Token, s.From, s.To, int64(s.Amount)
	}
	_, err := q.Exec(ctx, `
		INSERT INTO chainsim.transfers (tx_hash, log_index, token, from_address, to_address, amount)
		SELECT $1, u.log_index, u.token, u.from_address, u.to_address, u.amount
		FROM unnest($2::int[], $3::text[], $4::text[], $5::text[], $6::bigint[])
			AS u(log_index, token, from_address, to_address, amount)`,
		txHash, logIndexes, tokens, froms, tos, amounts)
	if err != nil {
		return fmt.Errorf("chain: insert transfers of %s: %w", txHash, err)
	}
	return nil
}

// includeMempool переносит весь мемпул в блок с хэшем blockHash.
func includeMempool(ctx context.Context, tx pgx.Tx, blockHash string) error {
	_, err := tx.Exec(ctx, `UPDATE chainsim.transfers SET block_hash = $1 WHERE block_hash IS NULL`, blockHash)
	if err != nil {
		return fmt.Errorf("chain: include mempool into block %s: %w", blockHash, err)
	}
	return nil
}

// deleteTransfersAbove удаляет переводы из блоков с номером больше number: транзакции исчезают совсем.
func deleteTransfersAbove(ctx context.Context, tx pgx.Tx, number int64) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM chainsim.transfers
		WHERE block_hash IN (SELECT hash FROM chainsim.blocks WHERE number > $1)`, number)
	if err != nil {
		return fmt.Errorf("chain: delete transfers above block %d: %w", number, err)
	}
	return nil
}

// deleteBlocksAbove удаляет блоки с номером больше number. Внешний ключ transfers.block_hash
// объявлен ON DELETE SET NULL, поэтому их переводы сами возвращаются в мемпул.
func deleteBlocksAbove(ctx context.Context, tx pgx.Tx, number int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM chainsim.blocks WHERE number > $1`, number)
	if err != nil {
		return fmt.Errorf("chain: delete blocks above %d: %w", number, err)
	}
	return nil
}

// selectTransfers читает переводы блока в порядке добавления в мемпул.
func selectTransfers(ctx context.Context, q querier, blockHash string) ([]Transfer, error) {
	rows, err := q.Query(ctx, `
		SELECT tx_hash, log_index, token, from_address, to_address, amount
		FROM chainsim.transfers WHERE block_hash = $1 ORDER BY id`, blockHash)
	if err != nil {
		return nil, fmt.Errorf("chain: select transfers of block %s: %w", blockHash, err)
	}
	transfers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Transfer])
	if err != nil {
		return nil, fmt.Errorf("chain: select transfers of block %s: %w", blockHash, err)
	}
	return transfers, nil
}
