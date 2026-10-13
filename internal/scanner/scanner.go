package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// maxBlocksPerTick — сколько новых блоков сканер читает за один заход. На длинной цепочке (первый
// запуск, долгий простой) проход иначе держал бы блокировку до самой головы и откладывал
// подтверждения до конца чтения. Прочитав столько блоков, проход подтверждает платежи по курсору
// и завершается, остальное дочитывают следующие проходы.
const maxBlocksPerTick = 200

// closeTimeout ограничивает закрытие соединения после ошибки прохода: ctx к этому времени
// мог быть отменён, а закрытие не должно зависнуть на оборванной сети.
const closeTimeout = 5 * time.Second

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
//  1. берёт из пула одно соединение и блокировку «сканер один» на нём (pg_try_advisory_lock):
//     если блокировка у другого экземпляра, проход пропускается без ошибки;
//  2. сверяет свой последний блок с сетью; при расхождении откатывается до общего предка и
//     переводит платежи выброшенных блоков в orphaned;
//  3. читает новые блоки по одному, от курсора до головы сети, но не больше maxBlocksPerTick:
//     переводы USDT на депозитные адреса счетов превращаются в платежи detected;
//  4. проверяет, что курсор (последний обработанный блок) всё ещё в сети, и переводит платежи,
//     набравшие N подтверждений от курсора, в aml_pending (с проводкой в холд);
//  5. снимает блокировку и возвращает соединение в пул.
//
// Вся работа с базой в шагах 2-4 идёт через соединение из шага 1, то самое, что держит
// блокировку. Оборвалось соединение — блокировка снята, и следующий запрос прохода падает:
// «зомби-прохода», который продолжает писать без блокировки, не бывает. После ошибки или
// паники соединение закрывается, а не возвращается в пул, чтобы блокировка не осталась
// висеть на нём.
//
// Повторный и параллельный вызов безопасны: курсор хранится в базе, блок записывается вместе
// с платежами одной транзакцией, а платёж уникален по (tx_hash, log_index). Вызывать можно
// из теста напрямую, без таймера.
func (s *Scanner) Tick(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("scanner: acquire connection: %w", err)
	}

	// В пул соединение уходит только чистым: без блокировки или с уже снятой. На остальных
	// путях, включая панику, его закрывает отложенный вызов.
	reusable := false
	defer func() {
		if reusable {
			conn.Release()
		} else {
			closeConn(ctx, conn)
		}
	}()

	acquired, err := tryLock(ctx, conn)
	if err != nil {
		return err
	}
	if !acquired {
		reusable = true // блокировки на соединении нет
		return nil      // другой экземпляр сканера уже работает
	}
	if err := s.pass(ctx, conn); err != nil {
		return err
	}
	if err := unlock(ctx, conn); err != nil {
		return err
	}
	reusable = true
	return nil
}

// pass выполняет шаги 2-4. Если сеть переписала цепочку во время чтения блоков, возвращается
// к сверке (шаг 2); число возвратов ограничено.
func (s *Scanner) pass(ctx context.Context, conn *pgxpool.Conn) error {
	for range maxRounds {
		if err := s.reconcile(ctx, conn); err != nil {
			return err
		}
		cursor, stable, err := s.scanNew(ctx, conn)
		if err != nil {
			return err
		}
		if !stable {
			continue
		}

		// Подтверждения считаются от своего проверенного блока, а не от головы сети: голова
		// могла измениться после сверки, и платёж из выброшенного блока нельзя продвигать по ней.
		confirmed, err := payments.ConfirmReady(ctx, conn, cursor, s.confirmations)
		if confirmed > 0 {
			s.log.Info("payments reached the confirmation threshold", "count", confirmed, "cursor", cursor)
		}
		return err
	}
	return ErrChainUnstable
}

// reconcile — шаг 2: сверка с сетью. Последний обработанный блок должен совпасть с блоком сети
// под тем же номером. Если не совпал или такого блока уже нет, идём назад по своим блокам до
// общего предка и откатываемся до него.
func (s *Scanner) reconcile(ctx context.Context, conn *pgxpool.Conn) error {
	last, found, err := selectLastBlock(ctx, conn)
	if err != nil {
		return err
	}
	if !found {
		return nil // своих блоков ещё нет: сверять нечего
	}
	same, err := s.onNetwork(ctx, last)
	if err != nil {
		return err
	}
	if same {
		return nil // последний блок совпал с сетью: реорга не было
	}

	// Общий предок: ближайший к концу свой блок, который есть в сети с тем же хэшем.
	// Нумерация с единицы, поэтому 0 значит «общего блока нет, откатываемся до начала».
	var ancestor int64
	for cur := last; ; {
		prev, found, err := selectBlockBefore(ctx, conn, cur.Number)
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
	return s.rollbackTo(ctx, conn, ancestor, last.Number)
}

// rollbackTo одной транзакцией стирает свои блоки выше ancestor и переводит платежи из выброшенных
// блоков в orphaned. lastNumber — номер самого высокого блока, который был у сканера (для лога).
func (s *Scanner) rollbackTo(ctx context.Context, conn *pgxpool.Conn, ancestor, lastNumber int64) error {
	var dropped, orphaned, confirmed int
	err := postgres.InTx(ctx, conn, func(tx pgx.Tx) error {
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

// scanNew — шаг 3: читает блоки по одному от курсора + 1 до головы сети, но не больше maxBlocksPerTick
// за раз. Возвращает номер курсора — последнего обработанного блока (0, если блоков нет): от него
// считаются подтверждения. stable = false, если сеть переписала цепочку во время чтения (блока уже
// нет, в том числе между заголовком и событиями, его parent_hash не совпал с нашим предыдущим
// блоком или сам курсор уже не лежит в сети): нужна новая сверка.
func (s *Scanner) scanNew(ctx context.Context, conn *pgxpool.Conn) (cursor int64, stable bool, err error) {
	// Верхняя граница: номер последнего блока сети. Сами блоки отсюда не читаем, это только
	// отметка, до которой идти.
	head, err := s.network.Head(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("scanner: head of the network: %w", err)
	}
	// Нижняя граница: наш последний обработанный блок, курсор. Пока своих блоков нет,
	// prev.Number = 0 и чтение начинается с первого блока.
	prev, hasPrev, err := selectLastBlock(ctx, conn)
	if err != nil {
		return 0, false, err
	}

	// Идём снизу вверх, от курсора + 1 к голове сети, но не дальше лимита на проход:
	// остальное дочитают следующие проходы. prev сдвигается вместе с чтением.
	until := min(head, prev.Number+maxBlocksPerTick)
	for number := prev.Number + 1; number <= until; number++ {
		b, found, err := s.network.Block(ctx, number)
		if err != nil {
			return 0, false, fmt.Errorf("scanner: block %d from the network: %w", number, err)
		}
		if !found || (hasPrev && b.ParentHash != prev.Hash) {
			return 0, false, nil
		}
		err = s.processBlock(ctx, conn, b)
		if errors.Is(err, chain.ErrBlockNotFound) {
			return 0, false, nil // блок выброшен между заголовком и событиями: та же переписанная цепочка
		}
		if err != nil {
			return 0, false, err
		}
		prev, hasPrev = b, true
	}

	// Курсор должен быть в сети и сейчас: цикл мог не выполниться (голова не выше курсора), а у
	// последнего прочитанного блока ещё нет потомка, по которому parent_hash выдал бы подмену.
	if hasPrev {
		same, err := s.onNetwork(ctx, prev)
		if err != nil {
			return 0, false, err
		}
		if !same {
			return 0, false, nil // курсора в сети уже нет: нужна новая сверка
		}
	}
	return prev.Number, true, nil
}

// processBlock обрабатывает блок целиком: запись о блоке и платежи по всем его переводам —
// одна транзакция базы. Блок обработан весь или не обработан вовсе, курсор не уходит вперёд
// платежей. События из сети читаются до открытия транзакции.
func (s *Scanner) processBlock(ctx context.Context, conn *pgxpool.Conn, b chain.Block) error {
	transfers, err := s.network.Transfers(ctx, b.Hash)
	if err != nil {
		return fmt.Errorf("scanner: transfers of block %d: %w", b.Number, err)
	}

	detected := 0
	err = postgres.InTx(ctx, conn, func(tx pgx.Tx) error {
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
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil // получатель — не депозитный адрес нашего счёта
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

// closeConn закрывает соединение, не возвращая его в пул: сессионная блокировка снимается вместе
// с ним. Ошибку закрытия возвращать некому: соединение уже не нужно.
func closeConn(ctx context.Context, conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}
