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
	// ErrUnknownVerdict — AML вернул вердикт, которого код не знает. Ни зачислять, ни отклонять
	// по такому ответу нельзя: платёж остаётся в aml_pending.
	ErrUnknownVerdict = errors.New("payments: unknown AML verdict")
	// ErrInconsistentState — статус платежа и журнал или возврат не сходятся: статус уже сменён
	// этим вызовом, а проводка или возврат под ключом платежа уже есть. Так быть не должно;
	// транзакция откатывается вместе со сменой статуса.
	ErrInconsistentState = errors.New("payments: payment status disagrees with the journal")
)
