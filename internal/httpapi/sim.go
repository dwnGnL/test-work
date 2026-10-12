package httpapi

import (
	"errors"
	"net/http"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/money"
)

const (
	// Сообщения, если тело запроса к ручке симулятора не того вида.
	transferBodyShape = `request body must be a JSON object with string fields "to", "from", "amount" and an optional string "token"`
	blocksBodyShape   = `request body must be a JSON object with an integer field "count"`
	reorgBodyShape    = `request body must be a JSON object with an integer field "depth" and an optional boolean "drop_transfers"`
	amlBodyShape      = `request body must be a JSON object with a boolean field "available"`
)

// simTransferRequest — тело POST /sim/transfers. Amount указателем, чтобы пропущенную сумму
// не принять за ноль: нулевой перевод допустим, но его нужно попросить явно.
type simTransferRequest struct {
	To     string        `json:"to"`
	From   string        `json:"from"`
	Amount *money.Amount `json:"amount"`
	Token  string        `json:"token"` // по умолчанию — контракт USDT из конфигурации
}

// simBlocksRequest — тело POST /sim/blocks.
type simBlocksRequest struct {
	Count int `json:"count"`
}

// simReorgRequest — тело POST /sim/reorg.
type simReorgRequest struct {
	Depth         int  `json:"depth"`
	DropTransfers bool `json:"drop_transfers"`
}

// simAMLRequest — тело POST /sim/aml. Available указателем, чтобы пропущенное поле не приняли
// за false: выключить AML нужно попросить явно.
type simAMLRequest struct {
	Available *bool `json:"available"`
}

// amlResponse — ответ POST /sim/aml: в каком состоянии теперь мок.
type amlResponse struct {
	Available bool `json:"available"`
}

// txHashResponse — ответ POST /sim/transfers.
type txHashResponse struct {
	TxHash string `json:"tx_hash"`
}

// headResponse — ответ POST /sim/blocks и POST /sim/reorg: новая голова цепочки.
// Пустая цепочка (после реорга на всю глубину) — number 0 и пустые хэши.
type headResponse struct {
	Head blockResponse `json:"head"`
}

type blockResponse struct {
	Number     int64  `json:"number"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parent_hash"`
}

// simTransfer обрабатывает POST /sim/transfers: одна транзакция с одним переводом уходит в
// мемпул симулятора, 201 с хэшем транзакции.
func (a *API) simTransfer(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[simTransferRequest](w, r)
	if err != nil {
		writeDecodeError(w, err, transferBodyShape)
		return
	}
	if req.Amount == nil {
		writeError(w, http.StatusBadRequest, codeInvalidAmount, "amount is required")
		return
	}
	token := req.Token
	if token == "" {
		token = a.usdt
	}

	txHash, err := a.sim.SubmitTx(r.Context(), []chain.TransferSpec{
		{Token: token, From: req.From, To: req.To, Amount: *req.Amount},
	})
	switch {
	case errors.Is(err, chain.ErrInvalidTransfer):
		writeError(w, http.StatusBadRequest, codeInvalidTransfer, err.Error())
	case err != nil:
		a.internalError(w, r, err)
	default:
		a.writeJSON(w, r, http.StatusCreated, txHashResponse{TxHash: txHash})
	}
}

// simBlocks обрабатывает POST /sim/blocks: добывает count блоков (первый забирает мемпул,
// остальные пустые — это и есть подтверждения), 201 с новой головой.
func (a *API) simBlocks(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[simBlocksRequest](w, r)
	if err != nil {
		writeDecodeError(w, err, blocksBodyShape)
		return
	}

	head, err := a.sim.Mine(r.Context(), req.Count)
	switch {
	case errors.Is(err, chain.ErrInvalidCount):
		writeError(w, http.StatusBadRequest, codeInvalidCount, err.Error())
	case err != nil:
		a.internalError(w, r, err)
	default:
		a.writeJSON(w, r, http.StatusCreated, toHeadResponse(head))
	}
}

// simReorg обрабатывает POST /sim/reorg: выбрасывает последние depth блоков, 200 с новой головой.
// drop_transfers = true — транзакции выброшенных блоков исчезают, иначе возвращаются в мемпул.
func (a *API) simReorg(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[simReorgRequest](w, r)
	if err != nil {
		writeDecodeError(w, err, reorgBodyShape)
		return
	}

	head, err := a.sim.Reorg(r.Context(), req.Depth, req.DropTransfers)
	switch {
	case errors.Is(err, chain.ErrInvalidDepth):
		writeError(w, http.StatusBadRequest, codeInvalidDepth, err.Error())
	case errors.Is(err, chain.ErrReorgTooDeep):
		writeError(w, http.StatusBadRequest, codeReorgTooDeep, err.Error())
	case err != nil:
		a.internalError(w, r, err)
	default:
		a.writeJSON(w, r, http.StatusOK, toHeadResponse(head))
	}
}

// simAML обрабатывает POST /sim/aml: включает или выключает AML-мок, 200 с новым состоянием.
// Пока мок выключен, платежи ждут проверки в aml_pending, деньги остаются в холде.
func (a *API) simAML(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[simAMLRequest](w, r)
	if err != nil {
		writeDecodeError(w, err, amlBodyShape)
		return
	}
	if req.Available == nil {
		writeError(w, http.StatusBadRequest, codeInvalidBody, amlBodyShape)
		return
	}

	a.aml.SetAvailable(*req.Available)
	a.writeJSON(w, r, http.StatusOK, amlResponse{Available: *req.Available})
}

func toHeadResponse(b chain.Block) headResponse {
	return headResponse{Head: blockResponse{Number: b.Number, Hash: b.Hash, ParentHash: b.ParentHash}}
}
