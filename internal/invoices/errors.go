package invoices

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	ErrInvalidPartnerID      = errors.New("invoices: partner id must be positive")
	ErrInvalidIdempotencyKey = errors.New("invoices: idempotency key must be 1 to 255 characters of valid UTF-8")
	ErrNonPositiveAmount     = errors.New("invoices: amount must be positive")
	ErrInvalidFeeBps         = errors.New("invoices: fee must be between 0 and 10000 basis points")
	ErrNonPositiveTTL        = errors.New("invoices: ttl must be at least one microsecond")
	// ErrIdempotencyConflict — ключ идемпотентности этого партнёра уже использован
	// с другими параметрами запроса.
	ErrIdempotencyConflict = errors.New("invoices: idempotency key is already used with a different request")
	// ErrNotFound — счёта нет или он принадлежит другому партнёру (снаружи это не различить),
	// либо id не в формате UUID.
	ErrNotFound = errors.New("invoices: invoice not found")
)
