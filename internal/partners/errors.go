package partners

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	ErrEmptyName     = errors.New("partners: empty name")
	ErrEmptyAPIKey   = errors.New("partners: empty api key")
	ErrInvalidFeeBps = errors.New("partners: fee must be between 0 and 10000 basis points")
	// ErrAPIKeyTaken — ключ уже принадлежит другому партнёру (в базе уникален его sha256).
	ErrAPIKeyTaken = errors.New("partners: api key is already taken")
	// ErrUnauthorized — ключ пустой или не принадлежит ни одному партнёру. Причины не различаем:
	// снаружи нельзя узнать, чем «неверный» ключ отличается от «неизвестного».
	ErrUnauthorized = errors.New("partners: unauthorized")
)
