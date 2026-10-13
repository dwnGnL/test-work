package refunds

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/money"
)

// Status — статус возврата. Те же значения разрешает CHECK в миграции 0007.
type Status string

// Статусы возврата. Остальные (awaiting_address, deferred, broadcast, confirmed, blocked) описаны
// в документе, в коде их нет: возврат не отправляется в сеть. Переход один:
//
//	created ──MarkReady──▶ ready_to_send
const (
	StatusCreated     Status = "created"       // возврат создан вместе с отказом по платежу
	StatusReadyToSend Status = "ready_to_send" // готов к отправке; дальше в настоящей системе шла бы транзакция
)

// Refund — возврат отправителю: отдельная сущность со своим статусом, один на платёж.
type Refund struct {
	ID        int64
	PaymentID int64
	ToAddress string // адрес отправителя платежа, в нижнем регистре
	Amount    money.Amount
	Status    Status
}

// CreateParams — параметры создания возврата.
type CreateParams struct {
	PaymentID int64
	ToAddress string // регистр не важен: при записи приводится к нижнему
	Amount    money.Amount
}

// Create создаёт возврат платежа в транзакции вызывающего: возврат появляется вместе с отказом
// по платежу (смена статуса и проводка в той же транзакции) или не появляется вовсе.
//
// Один платёж — один возврат. Если возврат по этому платежу уже есть, он возвращается как есть
// с created = false, ничего не записывается и статус не меняется (возврат в ready_to_send
// не откатывается в created). Параметры повтора с сохранённым возвратом не сверяются: он создан
// первым вызовом. Параллельные вызовы по одному платежу выстраивает уникальный индекс: проигравший
// ждёт коммита победителя. Расчёт на READ COMMITTED (по умолчанию в postgres.InTx): после
// ожидания чтение должно видеть закоммиченный возврат победителя.
func Create(ctx context.Context, tx pgx.Tx, p CreateParams) (r Refund, created bool, err error) {
	if err := p.validate(); err != nil {
		return Refund{}, false, err
	}
	p.ToAddress = strings.ToLower(p.ToAddress)

	r, inserted, err := insertRefund(ctx, tx, p)
	if err != nil {
		return Refund{}, false, err
	}
	if inserted {
		return r, true, nil
	}
	// Возврат уже есть: отдаём его, второй не создаётся.
	r, err = selectByPayment(ctx, tx, p.PaymentID)
	if err != nil {
		return Refund{}, false, err
	}
	return r, false, nil
}

// validate проверяет параметры, не обращаясь к базе.
func (p CreateParams) validate() error {
	switch {
	case p.PaymentID <= 0:
		return fmt.Errorf("%w: payment id must be positive, got %d", ErrInvalidParams, p.PaymentID)
	case p.ToAddress == "":
		return fmt.Errorf("%w: empty recipient address", ErrInvalidParams)
	case p.Amount <= 0:
		return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, p.Amount)
	}
	return nil
}

// MarkReady переводит возвраты из created в ready_to_send. Это шаг, после которого в настоящей
// системе шла бы отправка транзакции; в коде её нет по условию. Условное обновление
// (WHERE status = 'created'): повторный и параллельный вызов ничего не меняют и возвращают 0.
// Возвращает, сколько возвратов переведено этим вызовом.
func MarkReady(ctx context.Context, db Execer) (moved int, err error) {
	return markReady(ctx, db)
}

// ByPayments возвращает возвраты перечисленных платежей, ключ — id платежа. Платежи без
// возврата в карте отсутствуют. Нужна ответу API: возврат показывается рядом с отклонённым платежом.
func ByPayments(ctx context.Context, q Querier, paymentIDs []int64) (map[int64]Refund, error) {
	byPayment := make(map[int64]Refund, len(paymentIDs))
	if len(paymentIDs) == 0 {
		return byPayment, nil
	}
	list, err := selectByPayments(ctx, q, paymentIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		byPayment[r.PaymentID] = r
	}
	return byPayment, nil
}
