package refunds

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	// ErrInvalidParams — параметры Create заданы неверно (нет платежа или адреса получателя).
	ErrInvalidParams = errors.New("refunds: invalid parameters")
	// ErrNonPositiveAmount — сумма возврата не больше нуля.
	ErrNonPositiveAmount = errors.New("refunds: amount must be positive")
)
