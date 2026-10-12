package scanner

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	// ErrChainUnstable — за один проход сеть несколько раз переписала цепочку, пока сканер её читал.
	// Прогресс сохранён, следующий проход продолжит с сохранённого курсора.
	ErrChainUnstable = errors.New("scanner: the chain keeps changing while it is being read")
	// ErrLockLost — в конце прохода у соединения уже не было блокировки «сканер один».
	ErrLockLost = errors.New("scanner: the lock was lost during the pass")
)
