package chain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

const (
	// maxMineBlocks — сколько блоков можно добыть одним вызовом Mine.
	maxMineBlocks = 1000
	// hashBytes — длина хэша блока и транзакции в байтах.
	hashBytes = 32
	// zeroHash — parent_hash первого блока. Нулевого блока в симуляторе нет: нумерация идёт
	// с единицы, а пока блоков нет, Head возвращает 0. Так не нужно создавать генезис при
	// миграции и нет блока, который нельзя выбросить реоргом.
	zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"
)

var addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// TransferSpec — один перевод токена для SubmitTx.
type TransferSpec struct {
	Token  string // адрес контракта токена
	From   string
	To     string
	Amount money.Amount // ноль допустим: сеть пропускает и нулевые переводы
}

// Simulator — сеть-симулятор. Состояние (блоки, мемпул, переводы) лежит в схеме chainsim,
// поэтому «сеть» переживает рестарт сервиса. Реализует Chain и добавляет методы управления.
// Безопасен для параллельных вызовов: Mine и Reorg выстраиваются в очередь блокировкой базы.
type Simulator struct {
	pool *pgxpool.Pool
}

var _ Chain = (*Simulator)(nil)

// NewSimulator возвращает симулятор поверх пула. Схема chainsim создаётся миграциями.
func NewSimulator(pool *pgxpool.Pool) *Simulator {
	return &Simulator{pool: pool}
}

// SubmitTx отправляет одну транзакцию с событиями Transfer в мемпул и возвращает её хэш.
// Событие под номером i получает log_index = i. Адреса приводятся к нижнему регистру.
// Транзакция попадёт в блок при ближайшем Mine; все её события — в один блок.
// Пустой список, неверный адрес и отрицательная сумма — ErrInvalidTransfer.
func (s *Simulator) SubmitTx(ctx context.Context, transfers []TransferSpec) (txHash string, err error) {
	specs, err := normalizeSpecs(transfers)
	if err != nil {
		return "", err
	}
	txHash, err = newHash()
	if err != nil {
		return "", err
	}
	if err := insertTransfers(ctx, s.pool, txHash, specs); err != nil {
		return "", err
	}
	return txHash, nil
}

// Mine добывает count блоков (от 1 до maxMineBlocks) и возвращает последний. Первый блок
// забирает весь мемпул, остальные пустые: это и есть «добавить подтверждения».
// Параллельные вызовы идут по очереди, номера блоков идут подряд без дыр и повторов.
func (s *Simulator) Mine(ctx context.Context, count int) (head Block, err error) {
	if count < 1 || count > maxMineBlocks {
		return Block{}, fmt.Errorf("%w: got %d, want 1 to %d", ErrInvalidCount, count, maxMineBlocks)
	}

	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockChain(ctx, tx); err != nil {
			return err
		}
		tip, found, err := selectHead(ctx, tx)
		if err != nil {
			return err
		}
		if !found {
			tip.Hash = zeroHash // нулевого блока нет: первый блок ссылается на нулевой хэш
		}

		blocks := make([]Block, count)
		for i := range blocks {
			hash, err := newHash()
			if err != nil {
				return err
			}
			blocks[i] = Block{Number: tip.Number + 1, Hash: hash, ParentHash: tip.Hash}
			tip = blocks[i]
		}
		if err := insertBlocks(ctx, tx, blocks); err != nil {
			return err
		}
		if err := includeMempool(ctx, tx, blocks[0].Hash); err != nil {
			return err
		}
		head = blocks[count-1]
		return nil
	})
	if err != nil {
		return Block{}, err
	}
	return head, nil
}

// Reorg выбрасывает последние depth блоков (реорганизация цепочки) и возвращает новую голову;
// если выброшена вся цепочка, голова пустая (Block{}, номер 0).
//
//   - dropTransfers = false: транзакции выброшенных блоков возвращаются в мемпул и попадут
//     в следующий добытый блок, у которого другой хэш;
//   - dropTransfers = true: транзакции выброшенных блоков исчезают совсем.
//
// depth меньше единицы — ErrInvalidDepth, больше длины цепочки — ErrReorgTooDeep.
func (s *Simulator) Reorg(ctx context.Context, depth int, dropTransfers bool) (head Block, err error) {
	if depth < 1 {
		return Block{}, fmt.Errorf("%w: got %d, want at least 1", ErrInvalidDepth, depth)
	}

	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockChain(ctx, tx); err != nil {
			return err
		}
		tip, _, err := selectHead(ctx, tx) // блоков нет — номер 0
		if err != nil {
			return err
		}
		if int64(depth) > tip.Number {
			return fmt.Errorf("%w: depth %d, the chain has %d blocks", ErrReorgTooDeep, depth, tip.Number)
		}

		keep := tip.Number - int64(depth) // номер последнего блока, который остаётся
		if dropTransfers {
			if err := deleteTransfersAbove(ctx, tx, keep); err != nil {
				return err
			}
		}
		if err := deleteBlocksAbove(ctx, tx, keep); err != nil {
			return err
		}
		head, _, err = selectBlock(ctx, tx, keep)
		return err
	})
	if err != nil {
		return Block{}, err
	}
	return head, nil
}

// Head возвращает номер последнего блока; 0, если блоков ещё нет.
func (s *Simulator) Head(ctx context.Context) (int64, error) {
	head, _, err := selectHead(ctx, s.pool)
	return head.Number, err
}

// Block возвращает заголовок блока по номеру; found = false, если такого блока нет.
func (s *Simulator) Block(ctx context.Context, number int64) (Block, bool, error) {
	return selectBlock(ctx, s.pool, number)
}

// Transfers возвращает события Transfer блока по его хэшу. Неизвестный хэш — ErrBlockNotFound.
func (s *Simulator) Transfers(ctx context.Context, blockHash string) ([]Transfer, error) {
	transfers, err := selectTransfers(ctx, s.pool, blockHash)
	if err != nil {
		return nil, err
	}
	if len(transfers) > 0 {
		return transfers, nil
	}
	// Пусто бывает у пустого блока и у блока, которого нет. Различаем: сканер не должен
	// записать выброшенный блок как «обработанный без переводов».
	exists, err := blockExists(ctx, s.pool, blockHash)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrBlockNotFound, blockHash)
	}
	return nil, nil
}

// normalizeSpecs проверяет переводы и возвращает копию с адресами в нижнем регистре.
func normalizeSpecs(specs []TransferSpec) ([]TransferSpec, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("%w: a transaction needs at least one transfer", ErrInvalidTransfer)
	}
	out := make([]TransferSpec, len(specs))
	for i, s := range specs {
		if err := checkAddress(i, "token", s.Token); err != nil {
			return nil, err
		}
		if err := checkAddress(i, "from", s.From); err != nil {
			return nil, err
		}
		if err := checkAddress(i, "to", s.To); err != nil {
			return nil, err
		}
		if s.Amount < 0 {
			return nil, fmt.Errorf("%w: transfer %d: amount must not be negative", ErrInvalidTransfer, i)
		}
		out[i] = TransferSpec{
			Token:  strings.ToLower(s.Token),
			From:   strings.ToLower(s.From),
			To:     strings.ToLower(s.To),
			Amount: s.Amount,
		}
	}
	return out, nil
}

// checkAddress проверяет формат адреса поля field перевода с номером i.
func checkAddress(i int, field, addr string) error {
	if !addressRe.MatchString(addr) {
		return fmt.Errorf("%w: transfer %d: %s %q is not an Ethereum address (0x + 40 hex digits)",
			ErrInvalidTransfer, i, field, addr)
	}
	return nil
}

// newHash возвращает случайный хэш: 0x и 64 hex-символа в нижнем регистре.
func newHash() (string, error) {
	var b [hashBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("chain: generate hash: %w", err)
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}
