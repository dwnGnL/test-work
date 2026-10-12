package invoices

import (
	"time"

	"github.com/dwnGnL/test-work/internal/money"
)

// Статусы счёта. Они не хранятся: каждый раз выводятся из суммы зачисленных платежей и срока.
const (
	StatusPending       = "pending"        // ничего не зачислено, срок не вышел
	StatusPartiallyPaid = "partially_paid" // зачислена часть суммы
	StatusPaid          = "paid"           // зачислено не меньше суммы счёта
	StatusExpired       = "expired"        // срок вышел, суммы нет и платежей в обработке нет
)

// Status выводит статус счёта. Чистая функция: к базе не обращается, поэтому вызывающий
// сам приносит данные о платежах.
//
//   - credited — сумма платежей, уже зачисленных партнёру;
//   - inProgress — есть платежи в обработке (ждут подтверждений, AML), они ещё могут зачислиться;
//   - now — текущее время (для блоков сети — время блока).
//
// Порядок проверок важен. Оплаченный счёт остаётся paid и после срока: деньги получены.
// Просроченный счёт, по которому идёт обработка платежа, не expired: исход ещё не решён.
// Срок вышел, когда now строго позже ExpiresAt.
func Status(inv Invoice, credited money.Amount, inProgress bool, now time.Time) string {
	switch {
	case credited >= inv.Amount:
		return StatusPaid
	case now.After(inv.ExpiresAt) && !inProgress:
		return StatusExpired
	case credited > 0:
		return StatusPartiallyPaid
	default:
		return StatusPending
	}
}
