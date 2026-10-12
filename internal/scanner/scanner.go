package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

// maxRounds — сколько раз за один проход можно вернуться к сверке с сетью, если сеть
// переписала цепочку, пока сканер её читал. Больше — ErrChainUnstable.
const maxRounds = 3

// Scanner — сканер блоков. Создаётся через New; Tick делает один проход.
type Scanner struct {
	pool          *pgxpool.Pool
	network       chain.Chain
	confirmations int
	usdt          string // адрес контракта USDT в нижнем регистре
	log           *slog.Logger
}

// New возвращает сканер. confirmations — порог подтверждений N (не меньше 1), usdtContract —
// адрес контракта USDT; события других токенов игнорируются. Регистр адреса не важен.
func New(pool *pgxpool.Pool, network chain.Chain, confirmations int, usdtContract string, log *slog.Logger) *Scanner {
	return &Scanner{
		pool:          pool,
		network:       network,
		confirmations: confirmations,
		usdt:          strings.ToLower(usdtContract),
		log:           log,
	}
}

// Tick делает один проход сканера:
//  1. берёт блокировку «сканер один»: если она у другого экземпляра, проход пропускается без ошибки;
//  2. сверяет свой последний блок с сетью; при расхождении откатывается до общего предка и
//     переводит платежи выброшенных блоков в orphaned;
//  3. читает новые блоки по одному, от курсора до головы сети: переводы USDT на депозитные
//     адреса счетов превращаются в платежи detected;
//  4. переводит платежи, набравшие N подтверждений, в aml_pending (с проводкой в холд).
//
// Повторный и параллельный вызов безопасны: курсор хранится в базе, блок записывается вместе
// с платежами одной транзакцией, а платёж уникален по (tx_hash, log_index). Вызывать можно
// из теста напрямую, без таймера.
func (s *Scanner) Tick(ctx context.Context) error {
	// Блокировка живёт, пока открыта эта транзакция. Работы в ней нет: сама работа идёт в
	// собственных транзакциях на других соединениях, эта нужна только как «аренда» блокировки.
	return postgres.InTx(ctx, s.pool, func(lease pgx.Tx) error {
		acquired, err := tryLock(ctx, lease)
		if err != nil {
			return err
		}
		if !acquired {
			return nil // другой экземпляр сканера уже работает
		}
		return s.pass(ctx)
	})
}

// pass выполняет шаги 2-4. Если сеть переписала цепочку во время чтения блоков, возвращается
// к сверке (шаг 2); число возвратов ограничено.
func (s *Scanner) pass(ctx context.Context) error {
	for range maxRounds {
		if err := s.reconcile(ctx); err != nil {
			return err
		}
		head, stable, err := s.scanNew(ctx)
		if err != nil {
			return err
		}
		if !stable {
			continue
		}

		confirmed, err := payments.ConfirmReady(ctx, s.pool, head, s.confirmations)
		if confirmed > 0 {
			s.log.Info("payments reached the confirmation threshold", "count", confirmed, "head", head)
		}
		return err
	}
	return ErrChainUnstable
}

// reconcile — шаг 2: сверка с сетью. Последний обработанный блок должен совпасть с блоком сети
// под тем же номером. Если не совпал или такого блока уже нет, идём назад по своим блокам до
// общего предка и откатываемся до него.
func (s *Scanner) reconcile(ctx context.Context) error {
	last, found, err := selectLastBlock(ctx, s.pool)
	if err != nil || !found {
		return err // блоков ещё нет: сверять нечего
	}
	same, err := s.onNetwork(ctx, last)
	if err != nil || same {
		return err
	}

	// Общий предок: ближайший к концу свой блок, который есть в сети с тем же хэшем.
	// Нумерация с единицы, поэтому 0 значит «общего блока нет, откатываемся до начала».
	var ancestor int64
	for cur := last; ; {
		prev, found, err := selectBlockBefore(ctx, s.pool, cur.Number)
		if err != nil {
			return err
		}
		if !found {
			break
		}
		same, err := s.onNetwork(ctx, prev)
		if err != nil {
			return err
		}
		if same {
			ancestor = prev.Number
			break
		}
		cur = prev
	}
	return s.rollbackTo(ctx, ancestor, last.Number)
}

// rollbackTo одной транзакцией стирает свои блоки выше ancestor и переводит платежи из выброшенных
// блоков в orphaned. lastNumber — номер самого высокого блока, который был у сканера (для лога).
func (s *Scanner) rollbackTo(ctx context.Context, ancestor, lastNumber int64) error {
	var dropped, orphaned, confirmed int
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if dropped, err = deleteBlocksFrom(ctx, tx, ancestor+1); err != nil {
			return err
		}
		orphaned, confirmed, err = payments.OrphanFrom(ctx, tx, ancestor+1)
		return err
	})
	if err != nil {
		return fmt.Errorf("scanner: roll back to block %d: %w", ancestor, err)
	}

	s.log.Warn("reorg detected, rolled back to the common ancestor",
		"ancestor", ancestor, "scanned_up_to", lastNumber, "dropped_blocks", dropped, "orphaned_payments", orphaned)
	if confirmed > 0 {
		// Реорг глубже порога подтверждений: эти платежи уже в холде или дальше, и в коде такой реорг
		// не обрабатывается. Платежи не трогаем, чинить будет человек.
		s.log.Error("reorg is deeper than the confirmation threshold: payments past the threshold were not rolled back",
			"ancestor", ancestor, "affected_payments", confirmed, "confirmations", s.confirmations)
	}
	return nil
}

// onNetwork сообщает, есть ли в сети блок b: тот же номер и тот же хэш.
func (s *Scanner) onNetwork(ctx context.Context, b chain.Block) (bool, error) {
	remote, found, err := s.network.Block(ctx, b.Number)
	if err != nil {
		return false, fmt.Errorf("scanner: block %d from the network: %w", b.Number, err)
	}
	return found && remote.Hash == b.Hash, nil
}

// scanNew — шаг 3: читает блоки по одному от курсора + 1 до головы сети. Возвращает голову, на
// которой остановился. stable = false, если сеть переписала цепочку во время чтения (блока уже
// нет или его parent_hash не совпал с нашим предыдущим блоком): нужна новая сверка.
func (s *Scanner) scanNew(ctx context.Context) (head int64, stable bool, err error) {
	head, err = s.network.Head(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("scanner: head of the network: %w", err)
	}
	prev, hasPrev, err := selectLastBlock(ctx, s.pool) // без блоков prev.Number = 0: идём с первого
	if err != nil {
		return 0, false, err
	}

	for number := prev.Number + 1; number <= head; number++ {
		b, found, err := s.network.Block(ctx, number)
		if err != nil {
			return 0, false, fmt.Errorf("scanner: block %d from the network: %w", number, err)
		}
		if !found || (hasPrev && b.ParentHash != prev.Hash) {
			return head, false, nil
		}
		if err := s.processBlock(ctx, b); err != nil {
			return 0, false, err
		}
		prev, hasPrev = b, true
	}
	return head, true, nil
}

// processBlock обрабатывает блок целиком: запись о блоке и платежи по всем его переводам —
// одна транзакция базы. Блок обработан весь или не обработан вовсе, курсор не уходит вперёд
// платежей. События из сети читаются до открытия транзакции.
func (s *Scanner) processBlock(ctx context.Context, b chain.Block) error {
	transfers, err := s.network.Transfers(ctx, b.Hash)
	if err != nil {
		return fmt.Errorf("scanner: transfers of block %d: %w", b.Number, err)
	}

	detected := 0
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := insertBlock(ctx, tx, b); err != nil {
			return err
		}
		for _, t := range transfers {
			ok, err := s.detect(ctx, tx, b, t)
			if err != nil {
				return err
			}
			if ok {
				detected++
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanner: process block %d: %w", b.Number, err)
	}
	if detected > 0 {
		s.log.Info("block scanned, transfers detected", "block", b.Number, "transfers", detected)
	}
	return nil
}

// detect превращает перевод в платёж, если это USDT на депозитный адрес нашего счёта; detected
// сообщает, что платёж записан. Остальное пропускается: чужой токен (поддельный «USDT» с другим
// адресом контракта), нулевой перевод, получатель не наш.
func (s *Scanner) detect(ctx context.Context, tx pgx.Tx, b chain.Block, t chain.Transfer) (detected bool, err error) {
	if strings.ToLower(t.Token) != s.usdt || t.Amount <= 0 {
		return false, nil
	}
	inv, found, err := invoices.FindByDepositAddress(ctx, tx, t.To)
	if err != nil || !found {
		return false, err
	}

	err = payments.Detect(ctx, tx, payments.DetectParams{
		InvoiceID:   inv.ID,
		PartnerID:   inv.PartnerID,
		TxHash:      t.TxHash,
		LogIndex:    t.LogIndex,
		From:        t.From,
		Amount:      t.Amount,
		FeeBps:      inv.FeeBps, // ставка копируется из счёта
		BlockNumber: b.Number,
		BlockHash:   b.Hash,
	})
	return err == nil, err
}
