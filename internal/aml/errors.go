package aml

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	// ErrUnavailable — AML-провайдер не ответил. Это не вердикт: платёж остаётся на проверке,
	// деньги в холде, проверка повторяется позже.
	ErrUnavailable = errors.New("aml: provider is unavailable")
)
