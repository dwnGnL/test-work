package invoices

import (
	"time"

	"github.com/dwnGnL/test-work/internal/money"
)

// Статусы счёта. Они не хранятся: каждый раз выводятся из суммы полученных платежей и срока.
const (
	StatusPending       = "pending"        // ничего не получено, срок не вышел
	StatusPartiallyPaid = "partially_paid" // получена часть суммы
	StatusPaid          = "paid"           // получено не меньше суммы счёта
	StatusExpired       = "expired"        // срок вышел, сумма не набрана (в том числе получена часть), платежей в обработке нет
)

// Status выводит статус счёта. Чистая функция: к базе не обращается, поэтому вызывающий
// сам приносит данные о платежах.
//
//   - received — сумма зачисленных платежей ДО вычета комиссии (поле amount платежа, а не то,
//     что дошло до баланса партнёра): счёт на 100 оплачен, когда пришло 100, хотя партнёру
//     при ставке 1,5% зачислено 98,5;
//   - inProgress — есть платежи в обработке: замечены в сети или ждут ответа AML и ещё могут
//     зачислиться. Платёж из откатившегося блока в обработке не считается;
//   - now — текущее время (для блоков сети — время блока).
//
// Порядок проверок важен. Оплаченный счёт остаётся paid и после срока: деньги получены.
// Просроченный счёт, по которому идёт обработка платежа, не expired: исход ещё не решён.
// Срок вышел, когда now строго позже ExpiresAt.
func Status(inv Invoice, received money.Amount, inProgress bool, now time.Time) string {
	switch {
	case received >= inv.Amount:
		return StatusPaid
	case now.After(inv.ExpiresAt) && !inProgress:
		return StatusExpired
	case received > 0:
		return StatusPartiallyPaid
	default:
		return StatusPending
	}
}
