package payments

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

const (
	// maxFeeBps — 100% в базисных пунктах; то же ограничение стоит CHECK в базе.
	maxFeeBps = 10_000
	// holdKind — вид проводки шага 1 (платёж прошёл порог, деньги в холде).
	holdKind = "hold"
)

// Payment — платёж: один входящий перевод USDT на депозитный адрес счёта.
type Payment struct {
	ID        int64
	InvoiceID string // UUID счёта в текстовом виде
	PartnerID int64
	TxHash    string
	LogIndex  int // позиция события в транзакции; вместе с TxHash даёт ключ платежа
	From      string
	Amount    money.Amount // сумма перевода, до комиссии
	FeeBps    int          // ставка счёта, скопированная при обнаружении
	// BlockNumber и BlockHash — блок, в котором сейчас виден перевод. nil, когда блока нет
	// (платёж orphaned): блок — изменяемый атрибут платежа, а не часть его ключа.
	BlockNumber *int64
	BlockHash   *string
	Status      Status
}

// DetectParams — перевод, замеченный сканером в блоке.
type DetectParams struct {
	InvoiceID   string
	PartnerID   int64
	TxHash      string
	LogIndex    int
	From        string
	Amount      money.Amount
	FeeBps      int // ставка счёта; копируется в платёж
	BlockNumber int64
	BlockHash   string
}

// DB — то, что нужно ConfirmReady: начать транзакцию и читать. Подходят и пул (*pgxpool.Pool),
// и одно соединение (*pgxpool.Conn).
type DB interface {
	postgres.Beginner
	Querier
}

// Detect записывает перевод, замеченный в блоке. Ключ платежа — (TxHash, LogIndex), блок
// не входит в ключ, поэтому та же транзакция после реорга остаётся тем же платежом:
//   - платежа нет — создаётся со статусом detected;
//   - платёж в orphaned — возвращается в detected с новым блоком (транзакцию включили заново);
//   - платёж в detected — получает новые номер и хэш блока;
//   - платёж в aml_pending, credited, rejected — не меняется.
//
// Всё это один запрос INSERT ... ON CONFLICT, поэтому повтор, рестарт и параллельные вызовы
// не создают второй записи. Хэши и адрес приводятся к нижнему регистру: иначе тот же перевод
// в другой записи считался бы новым платежом. Пишет в транзакцию вызывающего: сканер
// записывает все переводы блока и сам блок одной транзакцией.
func Detect(ctx context.Context, tx pgx.Tx, p DetectParams) error {
	if err := p.validate(); err != nil {
		return err
	}
	p.TxHash = strings.ToLower(p.TxHash)
	p.From = strings.ToLower(p.From)
	p.BlockHash = strings.ToLower(p.BlockHash)
	return upsertDetected(ctx, tx, p)
}

// validate проверяет параметры, не обращаясь к базе.
func (p DetectParams) validate() error {
	switch {
	case p.InvoiceID == "":
		return fmt.Errorf("%w: empty invoice id", ErrInvalidParams)
	case p.PartnerID <= 0:
		return fmt.Errorf("%w: partner id must be positive, got %d", ErrInvalidParams, p.PartnerID)
	case p.TxHash == "":
		return fmt.Errorf("%w: empty tx hash", ErrInvalidParams)
	case p.LogIndex < 0:
		return fmt.Errorf("%w: log index must not be negative, got %d", ErrInvalidParams, p.LogIndex)
	case p.From == "":
		return fmt.Errorf("%w: empty sender address", ErrInvalidParams)
	case p.Amount <= 0:
		return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, p.Amount)
	case p.FeeBps < 0 || p.FeeBps > maxFeeBps:
		return fmt.Errorf("%w: got %d", ErrInvalidFeeBps, p.FeeBps)
	case p.BlockNumber < 0 || p.BlockHash == "":
		return fmt.Errorf("%w: block %d %q", ErrInvalidParams, p.BlockNumber, p.BlockHash)
	}
	return nil
}

// OrphanFrom переводит платежи detected из блоков с номером >= fromBlock в orphaned: их блок
// выброшен реоргом, блок у платежа обнуляется. Это шаг сканера при реорге до порога; проводок
// у таких платежей нет, поэтому откатывать нечего.
//
// Платежи, уже прошедшие порог (aml_pending, credited, rejected), не меняются, а только считаются
// в affectedConfirmed: реорг глубже порога в коде не обрабатывается (решение в NOTES.md), и
// вызывающий пишет об этом ошибку в лог. Пишет в транзакцию вызывающего.
func OrphanFrom(ctx context.Context, tx pgx.Tx, fromBlock int64) (orphaned, affectedConfirmed int, err error) {
	orphaned, err = orphanDetected(ctx, tx, fromBlock)
	if err != nil {
		return 0, 0, err
	}
	affectedConfirmed, err = countPastThreshold(ctx, tx, fromBlock)
	if err != nil {
		return 0, 0, err
	}
	return orphaned, affectedConfirmed, nil
}

// ConfirmReady переводит платежи detected, набравшие confirmations подтверждений, в aml_pending
// и проводит шаг 1: hold (дебет assets:deposits, кредит hold:aml на сумму платежа). Блок B
// имеет одно подтверждение, когда он последний (B = head), и N, когда head >= B + N - 1.
// До порога проводок нет вообще, поэтому реорг до порога не оставляет «висящих» денег.
//
// Каждый платёж обрабатывается в отдельной транзакции: условная смена статуса
// (WHERE status = 'detected') и проводка с ключом {payment_id}:1 коммитятся вместе. Параллельные
// и повторные вызовы безопасны: платёж переводит только тот вызов, у которого изменилась строка,
// остальные его пропускают, а ключ проводки не даёт записать шаг дважды.
//
// Возвращает, сколько платежей переведено этим вызовом. Ошибка по одному платежу не мешает
// остальным: они обрабатываются, а ошибки возвращаются вместе (errors.Join). Платёж с ошибкой
// остаётся в detected, следующий вызов возьмёт его снова.
func ConfirmReady(ctx context.Context, db DB, head int64, confirmations int) (confirmed int, err error) {
	if confirmations < 1 {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidConfirmations, confirmations)
	}
	maxBlock := head - int64(confirmations) + 1 // самый высокий блок, у которого уже N подтверждений

	ids, err := selectReady(ctx, db, maxBlock)
	if err != nil {
		return 0, err
	}

	var errs []error
	for _, id := range ids {
		moved, err := confirmOne(ctx, db, id, maxBlock)
		if err != nil {
			errs = append(errs, fmt.Errorf("payments: confirm payment %d: %w", id, err))
			if ctx.Err() != nil {
				break // остановка сервиса: остальные платежи возьмёт следующий запуск
			}
			continue
		}
		if moved {
			confirmed++
		}
	}
	return confirmed, errors.Join(errs...)
}

// confirmOne переводит один платёж в aml_pending и проводит hold в одной транзакции.
// moved = false, если платёж уже обработан или больше не готов: тогда проводки нет.
func confirmOne(ctx context.Context, db DB, id, maxBlock int64) (moved bool, err error) {
	err = postgres.InTx(ctx, db, func(tx pgx.Tx) error {
		amount, ok, err := markAMLPending(ctx, tx, id, maxBlock)
		if err != nil || !ok {
			return err
		}
		moved = true
		// posted = false значит, что проводка с этим ключом и содержимым уже есть; это не ошибка,
		// статус всё равно должен сойтись с журналом. Другое содержимое под ключом — ErrKeyConflict.
		_, err = ledger.Post(ctx, tx, holdPosting(id, amount))
		return err
	})
	if err != nil {
		return false, err // транзакция откатилась вместе со сменой статуса
	}
	return moved, nil
}

// holdPosting — проводка шага 1: платёж прошёл порог, деньги из депозитов в холд AML.
// Ключ {payment_id}:1 одинаков при любом повторе.
func holdPosting(paymentID int64, amount money.Amount) ledger.Posting {
	return ledger.Posting{
		Key:  strconv.FormatInt(paymentID, 10) + ":1",
		Kind: holdKind,
		Lines: []ledger.Line{
			ledger.Debit(ledger.AccountDeposits, amount),
			ledger.Credit(ledger.AccountHoldAML, amount),
		},
	}
}

// Summary возвращает по счёту сумму полученных платежей и признак «есть платежи в обработке».
//   - received — сумма поля amount платежей в credited, то есть ДО вычета комиссии: именно её
//     сравнивает со счётом invoices.Status;
//   - inProgress — есть платежи в detected или aml_pending. Платёж orphaned в обработке не
//     считается, rejected тоже: деньги по ним не придут.
func Summary(ctx context.Context, q Querier, invoiceID string) (received money.Amount, inProgress bool, err error) {
	return selectSummary(ctx, q, invoiceID)
}

// ListByInvoice возвращает все платежи счёта в порядке создания, включая orphaned.
func ListByInvoice(ctx context.Context, q Querier, invoiceID string) ([]Payment, error) {
	return selectByInvoice(ctx, q, invoiceID)
}
