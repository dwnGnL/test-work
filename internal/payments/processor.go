package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/refunds"
)

const (
	// amlBatchSize — сколько платежей обработчик захватывает за один проход.
	amlBatchSize = 20
	// amlFirstPause — пауза до следующей проверки после первого захвата. С каждым захватом она
	// удваивается (5 с, 10 с, 20 с ...), но не дольше amlMaxPause.
	amlFirstPause = 5 * time.Second
	amlMaxPause   = 5 * time.Minute

	// Виды проводок шага 2 («решение по платежу»); ключ у них один, см. decisionKey.
	creditKind = "credit"
	refundKind = "refund"
)

// Processor — обработчик платежей после порога подтверждений: проверяет отправителя в AML и
// принимает решение по платежу — зачисляет партнёру за вычетом комиссии или отклоняет и создаёт
// возврат. Создаётся через NewProcessor; Tick делает один проход.
type Processor struct {
	pool    *pgxpool.Pool
	checker aml.Checker
	log     *slog.Logger
	now     func() time.Time // часы; подменяются в тестах
}

// NewProcessor возвращает обработчик платежей.
func NewProcessor(pool *pgxpool.Pool, checker aml.Checker, log *slog.Logger) *Processor {
	return &Processor{pool: pool, checker: checker, log: log, now: time.Now}
}

// Tick делает один проход:
//  1. захват: берёт пачку платежей aml_pending, чья очередь проверки пришла, увеличивает им
//     счётчик попыток и откладывает следующую попытку (см. claimForAML);
//  2. проверка: для каждого платежа спрашивает AML вне транзакции базы. Ошибка AML пишется в лог,
//     платёж остаётся в aml_pending, деньги в холде, баланс партнёра не меняется; платёж вернётся
//     в работу, когда истечёт пауза;
//  3. решение: одной транзакцией меняет статус (условно), проводит шаг 2 и, если риск высокий,
//     создаёт возврат;
//  4. переводит созданные возвраты в ready_to_send.
//
// Ошибка по одному платежу не мешает остальным: они обрабатываются, ошибки возвращаются вместе
// (errors.Join). Недоступность AML ошибкой прохода не считается: это штатное ожидание.
//
// Повторный и параллельный вызов безопасны: платёж захватывает один воркер, решение принимает
// тот, у кого изменилась строка, а ключ проводки не даёт провести шаг дважды. Вызывать можно из
// теста напрямую, без таймера.
func (p *Processor) Tick(ctx context.Context) error {
	claimed, err := claimForAML(ctx, p.pool, p.now(), amlBatchSize, amlFirstPause, amlMaxPause)
	if err != nil {
		return err
	}

	var errs []error
	for _, pay := range claimed {
		if err := p.process(ctx, pay); err != nil {
			errs = append(errs, fmt.Errorf("payments: process payment %d: %w", pay.ID, err))
			if ctx.Err() != nil {
				break // остановка сервиса: платёж вернётся в работу после паузы
			}
		}
	}

	// Возвраты, созданные этим или прошлым проходом (если тот упал до этого шага).
	if _, err := refunds.MarkReady(ctx, p.pool); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// process проверяет один захваченный платёж в AML и принимает решение.
func (p *Processor) process(ctx context.Context, pay Payment) error {
	// Внешний вызов вне транзакции базы: ответ может прийти не сразу, а открытая транзакция
	// держала бы соединение и блокировки.
	verdict, err := p.checker.Check(ctx, pay.From)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.log.Warn("AML check failed, payment stays in aml_pending",
			"payment_id", pay.ID, "err", err)
		return nil
	}

	decided, err := p.decide(ctx, pay, verdict)
	if err != nil {
		return err
	}
	if !decided {
		p.log.Info("payment was already decided by another worker", "payment_id", pay.ID)
		return nil
	}
	p.log.Info("payment decided", "payment_id", pay.ID, "verdict", verdict)
	return nil
}

// decide принимает решение по платежу одной транзакцией базы. decided = false, если платёж
// уже решён кем-то другим: тогда ни проводок, ни возврата, ни ошибки.
func (p *Processor) decide(ctx context.Context, pay Payment, verdict aml.Verdict) (decided bool, err error) {
	status, posting, err := decision(pay, verdict)
	if err != nil {
		return false, err
	}

	err = postgres.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		// Условная смена статуса: строка не изменилась — платёж уже решён, выходим без проводок.
		moved, err := markDecided(ctx, tx, pay.ID, status, verdict, p.now())
		if err != nil || !moved {
			return err
		}
		decided = true

		// Дальше статус уже сменён этим вызовом, поэтому любой сбой — ошибка и откат всей
		// транзакции. Проводка уже под ключом платежа (posted = false) или с другим содержимым
		// (ErrKeyConflict) значит, что статус и журнал разошлись; такого быть не должно.
		posted, err := ledger.Post(ctx, tx, posting)
		if err != nil {
			return fmt.Errorf("post %s of payment %d: %w", posting.Kind, pay.ID, err)
		}
		if !posted {
			return fmt.Errorf("%w: posting %q of payment %d already exists", ErrInconsistentState, posting.Key, pay.ID)
		}

		if verdict != aml.VerdictHigh {
			return nil
		}
		_, created, err := refunds.Create(ctx, tx, refunds.CreateParams{
			PaymentID: pay.ID, ToAddress: pay.From, Amount: pay.Amount,
		})
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("%w: refund of payment %d already exists", ErrInconsistentState, pay.ID)
		}
		return nil
	})
	if err != nil {
		return false, err // транзакция откатилась вместе со сменой статуса
	}
	return decided, nil
}

// decisionKey — ключ проводки шага 2. Он один на оба исхода: зачисление и возврат не могут
// быть проведены оба, даже если два воркера получили разные ответы AML.
func decisionKey(paymentID int64) string {
	return strconv.FormatInt(paymentID, 10) + ":2"
}

// decision выбирает по вердикту новый статус платежа и проводку шага 2. К базе не обращается.
func decision(pay Payment, verdict aml.Verdict) (Status, ledger.Posting, error) {
	switch verdict {
	case aml.VerdictLow:
		posting, err := creditPosting(pay)
		return StatusCredited, posting, err
	case aml.VerdictHigh:
		return StatusRejected, refundPosting(pay), nil
	default:
		return "", ledger.Posting{}, fmt.Errorf("%w: %q", ErrUnknownVerdict, verdict)
	}
}

// creditPosting — шаг 2a: низкий риск. Из холда на счёт партнёра amount − fee и на счёт
// комиссии fee. Нулевую строку проводка не принимает, поэтому нулевая комиссия (или стопроцентная,
// когда партнёру достаётся ноль) строкой не добавляется.
func creditPosting(pay Payment) (ledger.Posting, error) {
	fee, err := money.FeeBps(pay.Amount, pay.FeeBps)
	if err != nil {
		return ledger.Posting{}, fmt.Errorf("fee of payment %d: %w", pay.ID, err)
	}
	net := pay.Amount - fee

	lines := []ledger.Line{ledger.Debit(ledger.AccountHoldAML, pay.Amount)}
	if net > 0 {
		lines = append(lines, ledger.Credit(ledger.PartnerAccount(pay.PartnerID), net))
	}
	if fee > 0 {
		lines = append(lines, ledger.Credit(ledger.AccountRevenueFees, fee))
	}
	return ledger.Posting{Key: decisionKey(pay.ID), Kind: creditKind, Lines: lines}, nil
}

// refundPosting — шаг 2b: высокий риск. Вся сумма из холда в обязательства перед отправителем.
func refundPosting(pay Payment) ledger.Posting {
	return ledger.Posting{
		Key:  decisionKey(pay.ID),
		Kind: refundKind,
		Lines: []ledger.Line{
			ledger.Debit(ledger.AccountHoldAML, pay.Amount),
			ledger.Credit(ledger.AccountRefundsPayable, pay.Amount),
		},
	}
}
