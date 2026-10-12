package ledger

import (
	"errors"
	"fmt"
)

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	ErrEmptyKey          = errors.New("ledger: empty idempotency key")
	ErrEmptyKind         = errors.New("ledger: empty posting kind")
	ErrTooFewLines       = errors.New("ledger: posting needs at least two lines")
	ErrBadSide           = errors.New("ledger: line side must be debit or credit")
	ErrNonPositiveAmount = errors.New("ledger: line amount must be positive")
	ErrUnbalanced        = errors.New("ledger: lines do not sum to zero")
	ErrUnknownAccount    = errors.New("ledger: unknown account")
	ErrKeyConflict       = errors.New("ledger: idempotency key is already used with different content")
	// ErrBooksUnbalanced — сумма всех проводок журнала не равна нулю (CheckBalanced).
	ErrBooksUnbalanced = errors.New("ledger: books are not balanced")
)

// KeyConflictError уточняет ErrKeyConflict: под ключом уже записана проводка вида StoredKind.
// Обработчику платежей это нужно, чтобы отличить штатный исход гонки (по платежу уже
// проведён другой исход: зачисление вместо возврата или наоборот) от ошибки в содержимом.
type KeyConflictError struct {
	Key        string
	StoredKind string
}

func (e *KeyConflictError) Error() string {
	return fmt.Sprintf("%v: key %q, stored kind %q", ErrKeyConflict, e.Key, e.StoredKind)
}

// Is сохраняет проверку errors.Is(err, ErrKeyConflict).
func (e *KeyConflictError) Is(target error) bool { return target == ErrKeyConflict }
