package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/refunds"
)

const (
	// idempotencyHeader — заголовок с ключом идемпотентности при создании счёта.
	idempotencyHeader = "Idempotency-Key"
	// maxBodyBytes — предел тела запроса; настоящее тело занимает десятки байт.
	maxBodyBytes = 1 << 10
	// invoiceBodyShape — сообщение, если тело запроса на создание счёта не того вида.
	invoiceBodyShape = `request body must be a JSON object with a single string field "amount"`
)

// createInvoiceRequest — тело POST /v1/invoices.
type createInvoiceRequest struct {
	Amount money.Amount `json:"amount"` // только JSON-строка: "100.000000"
}

// invoiceResponse — счёт в ответе API. Суммы строками (money.Amount кодируется строкой),
// времена в RFC 3339, UTC. Payments — все платежи счёта, включая откатившиеся (orphaned).
type invoiceResponse struct {
	ID             string            `json:"id"`
	Amount         money.Amount      `json:"amount"`
	AmountReceived money.Amount      `json:"amount_received"`
	Status         string            `json:"status"`
	DepositAddress string            `json:"deposit_address"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Payments       []paymentResponse `json:"payments"`
}

// paymentResponse — платёж в ответе по счёту. BlockNumber равен null, пока у платежа нет блока
// (платёж orphaned: блок выброшен реоргом). Refund равен null, пока платёж не отклонён AML
// (статус rejected): тогда по нему создан возврат.
type paymentResponse struct {
	TxHash      string          `json:"tx_hash"`
	Amount      money.Amount    `json:"amount"`
	Status      string          `json:"status"`
	BlockNumber *int64          `json:"block_number"`
	Refund      *refundResponse `json:"refund"`
}

// refundResponse — возврат отклонённого платежа: куда, сколько и на каком он шаге.
type refundResponse struct {
	Status    string       `json:"status"`
	ToAddress string       `json:"to_address"`
	Amount    money.Amount `json:"amount"`
}

// balanceResponse — тело GET /v1/balance.
type balanceResponse struct {
	Balance money.Amount `json:"balance"`
}

// createInvoice обрабатывает POST /v1/invoices: 201 для нового счёта, 200 для повтора
// с тем же ключом и телом, 422 для того же ключа с другим телом.
func (a *API) createInvoice(w http.ResponseWriter, r *http.Request) {
	partner, ok := a.partnerFrom(w, r)
	if !ok {
		return
	}
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		writeError(w, http.StatusBadRequest, codeIdempotencyKeyRequired, idempotencyHeader+" header is required")
		return
	}
	req, err := decodeBody[createInvoiceRequest](w, r)
	if err != nil {
		writeDecodeError(w, err, invoiceBodyShape)
		return
	}

	inv, created, err := invoices.Create(r.Context(), a.db, invoices.CreateParams{
		PartnerID:      partner.ID,
		IdempotencyKey: key,
		Amount:         req.Amount,
		FeeBps:         partner.FeeBps, // ставку берём у партнёра, а не из запроса
		TTL:            a.invoiceTTL,
	})
	switch {
	case errors.Is(err, invoices.ErrInvalidIdempotencyKey):
		writeError(w, http.StatusBadRequest, codeInvalidIdempotencyKey, idempotencyHeader+" must be 1 to 255 characters")
	case errors.Is(err, invoices.ErrNonPositiveAmount):
		writeError(w, http.StatusBadRequest, codeInvalidAmount, "amount must be greater than zero")
	case errors.Is(err, invoices.ErrIdempotencyConflict):
		writeError(w, http.StatusUnprocessableEntity, codeIdempotencyConflict,
			idempotencyHeader+" was already used with a different request body")
	case err != nil:
		a.internalError(w, r, err)
	case created:
		a.writeInvoice(w, r, http.StatusCreated, inv)
	default:
		a.writeInvoice(w, r, http.StatusOK, inv)
	}
}

// getInvoice обрабатывает GET /v1/invoices/{id}. Чужой счёт — тот же 404, что и несуществующий.
func (a *API) getInvoice(w http.ResponseWriter, r *http.Request) {
	partner, ok := a.partnerFrom(w, r)
	if !ok {
		return
	}
	inv, err := invoices.Get(r.Context(), a.db, partner.ID, r.PathValue("id"))
	if errors.Is(err, invoices.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "invoice not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeInvoice(w, r, http.StatusOK, inv)
}

// getBalance обрабатывает GET /v1/balance: баланс только того партнёра, что вошёл.
func (a *API) getBalance(w http.ResponseWriter, r *http.Request) {
	partner, ok := a.partnerFrom(w, r)
	if !ok {
		return
	}
	balance, err := ledger.PartnerBalance(r.Context(), a.db, partner.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, balanceResponse{Balance: balance})
}

// writeInvoice отвечает счётом, собранным по данным модуля payments.
func (a *API) writeInvoice(w http.ResponseWriter, r *http.Request, status int, inv invoices.Invoice) {
	resp, err := a.toInvoiceResponse(r.Context(), inv)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeJSON(w, r, status, resp)
}

// toInvoiceResponse собирает ответ по счёту: сумма полученного (платежи credited, до комиссии)
// и статус считаются по payments.Summary, список платежей берётся из payments.ListByInvoice,
// возвраты отклонённых платежей — из refunds.ByPayments. Это отдельные чтения, поэтому во время
// смены статуса платежа они могут разойтись на один запрос; следующий запрос покажет
// согласованное состояние.
func (a *API) toInvoiceResponse(ctx context.Context, inv invoices.Invoice) (invoiceResponse, error) {
	received, inProgress, err := payments.Summary(ctx, a.db, inv.ID)
	if err != nil {
		return invoiceResponse{}, err
	}
	list, err := payments.ListByInvoice(ctx, a.db, inv.ID)
	if err != nil {
		return invoiceResponse{}, err
	}

	var rejected []int64 // возврат есть только у отклонённых платежей
	for _, p := range list {
		if p.Status == payments.StatusRejected {
			rejected = append(rejected, p.ID)
		}
	}
	refundOf, err := refunds.ByPayments(ctx, a.db, rejected)
	if err != nil {
		return invoiceResponse{}, err
	}

	items := make([]paymentResponse, len(list)) // пустой массив, а не null, когда платежей нет
	for i, p := range list {
		items[i] = paymentResponse{
			TxHash: p.TxHash, Amount: p.Amount, Status: string(p.Status), BlockNumber: p.BlockNumber,
			Refund: toRefundResponse(refundOf, p.ID),
		}
	}
	return invoiceResponse{
		ID:             inv.ID,
		Amount:         inv.Amount,
		AmountReceived: received,
		Status:         invoices.Status(inv, received, inProgress, a.now()),
		DepositAddress: inv.DepositAddress,
		ExpiresAt:      inv.ExpiresAt.UTC(),
		CreatedAt:      inv.CreatedAt.UTC(),
		Payments:       items,
	}, nil
}

// toRefundResponse возвращает возврат платежа paymentID или nil, если возврата нет.
func toRefundResponse(refundOf map[int64]refunds.Refund, paymentID int64) *refundResponse {
	r, ok := refundOf[paymentID]
	if !ok {
		return nil
	}
	return &refundResponse{Status: string(r.Status), ToAddress: r.ToAddress, Amount: r.Amount}
}

// decodeBody строго разбирает тело запроса в T: размер ограничен, неизвестные поля и данные
// после объекта не допускаются. Сумма принимается только JSON-строкой (см. money.Amount).
func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	var zero, req T
	if err := dec.Decode(&req); err != nil {
		return zero, err
	}
	// После объекта допустимы только пробелы: следующий токен обязан быть концом тела.
	_, err := dec.Token()
	switch {
	case errors.Is(err, io.EOF):
		return req, nil
	case err == nil:
		return zero, errTrailingData
	default:
		// Мусор вместо JSON или превышение размера на втором чтении.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return zero, err
		}
		return zero, errTrailingData
	}
}

// writeDecodeError переводит ошибку разбора тела в 400 (или 413 для слишком большого тела).
// shape — сообщение для тела не того вида (неизвестное поле, не тот тип, null).
func writeDecodeError(w http.ResponseWriter, err error, shape string) {
	var (
		maxErr    *http.MaxBytesError
		syntaxErr *json.SyntaxError
	)
	switch {
	case errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body is too large")
	case errors.Is(err, io.EOF):
		writeError(w, http.StatusBadRequest, codeInvalidBody, "request body is empty")
	case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &syntaxErr):
		writeError(w, http.StatusBadRequest, codeInvalidBody, "request body is not valid JSON")
	case errors.Is(err, money.ErrSyntax), errors.Is(err, money.ErrPrecision), errors.Is(err, money.ErrOverflow):
		writeError(w, http.StatusBadRequest, codeInvalidAmount, "amount: "+err.Error())
	default:
		// Неизвестное поле, сумма не строкой, null, не тот тип. Текст ошибки декодера наружу
		// не отдаём: в нём имена внутренних типов Go.
		writeError(w, http.StatusBadRequest, codeInvalidBody, shape)
	}
}
