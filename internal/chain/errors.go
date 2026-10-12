package chain

import "errors"

// Ошибки пакета. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	// ErrInvalidTransfer — перевод для SubmitTx задан неверно: пустой список, адрес не в формате
	// 0x + 40 hex-символов, отрицательная сумма.
	ErrInvalidTransfer = errors.New("chain: invalid transfer")
	// ErrInvalidCount — Mine вызван с числом блоков вне допустимых границ.
	ErrInvalidCount = errors.New("chain: invalid block count")
	// ErrInvalidDepth — Reorg вызван с глубиной меньше единицы.
	ErrInvalidDepth = errors.New("chain: invalid reorg depth")
	// ErrReorgTooDeep — Reorg просит выбросить больше блоков, чем есть в цепочке.
	ErrReorgTooDeep = errors.New("chain: reorg is deeper than the chain")
	// ErrBlockNotFound — блока с таким хэшем нет (его могли выбросить при реорге). Так же отвечает
	// eth_getLogs по неизвестному blockHash: пустой список здесь означал бы «в блоке нет переводов».
	ErrBlockNotFound = errors.New("chain: block not found")
)
