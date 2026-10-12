package chain

import (
	"context"

	"github.com/dwnGnL/test-work/internal/money"
)

// Block — заголовок блока. Хэши в нижнем регистре: 0x и 64 hex-символа.
type Block struct {
	Number     int64
	Hash       string
	ParentHash string
}

// Transfer — событие Transfer токена ERC-20 внутри блока. Адреса в нижнем регистре.
type Transfer struct {
	TxHash   string
	LogIndex int // позиция события внутри транзакции (receipt.logs), с нуля
	Token    string
	From     string
	To       string
	Amount   money.Amount
}

// Chain — всё, что сканеру нужно от сети. Три вызова повторяют JSON-RPC Ethereum:
// eth_blockNumber, eth_getBlockByNumber и eth_getLogs по blockHash. Реализация в проекте
// одна, Simulator; настоящую сеть можно подключить, не меняя сканер.
type Chain interface {
	// Head возвращает номер последнего блока; 0, если блоков ещё нет (нумерация с единицы).
	Head(ctx context.Context) (int64, error)
	// Block возвращает заголовок блока по номеру. found = false, если такого блока нет:
	// номер больше головы или блок выброшен при реорге.
	Block(ctx context.Context, number int64) (b Block, found bool, err error)
	// Transfers возвращает события Transfer блока по его хэшу, а не по номеру: номер после
	// реорга указывает на другой блок. Неизвестный хэш — ErrBlockNotFound.
	Transfers(ctx context.Context, blockHash string) ([]Transfer, error)
}
