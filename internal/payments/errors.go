package payments

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	// ErrInvalidParams — параметры Detect заданы неверно (пустой хэш, отрицательный номер и т. п.).
	ErrInvalidParams     = errors.New("payments: invalid parameters")
	ErrNonPositiveAmount = errors.New("payments: amount must be positive")
	ErrInvalidFeeBps     = errors.New("payments: fee must be between 0 and 10000 basis points")
	// ErrInvalidConfirmations — число подтверждений для ConfirmReady меньше единицы.
	ErrInvalidConfirmations = errors.New("payments: confirmations must be at least 1")
)
