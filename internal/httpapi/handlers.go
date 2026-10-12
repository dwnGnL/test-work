package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
)

const (
	// idempotencyHeader — заголовок с ключом идемпотентности при создании счёта.
	idempotencyHeader = "Idempotency-Key"
	// maxBodyBytes — предел тела запроса; настоящее тело занимает десятки байт.
	maxBodyBytes = 1 << 10
)

// createInvoiceRequest — тело POST /v1/invoices.
type createInvoiceRequest struct {
	Amount money.Amount `json:"amount"` // только JSON-строка: "100.000000"
}

// invoiceResponse — счёт в ответе API. Суммы строками (money.Amount кодируется строкой),
// времена в RFC 3339, UTC.
type invoiceResponse struct {
	ID             string       `json:"id"`
	Amount         money.Amount `json:"amount"`
	AmountReceived money.Amount `json:"amount_received"`
	Status         string       `json:"status"`
	DepositAddress string       `json:"deposit_address"`
	ExpiresAt      time.Time    `json:"expires_at"`
	CreatedAt      time.Time    `json:"created_at"`
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
	req, err := decodeCreateRequest(w, r)
	if err != nil {
		writeDecodeError(w, err)
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
		a.writeJSON(w, r, http.StatusCreated, a.invoiceResponse(inv))
	default:
		a.writeJSON(w, r, http.StatusOK, a.invoiceResponse(inv))
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
	a.writeJSON(w, r, http.StatusOK, a.invoiceResponse(inv))
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

// invoiceResponse собирает ответ по счёту.
func (a *API) invoiceResponse(inv invoices.Invoice) invoiceResponse {
	// Платежей пока нет: зачисленная сумма нулевая, в обработке ничего нет. Когда появится
	// модуль платежей, сюда придут его данные (сумма зачисленных платежей и признак
	// «есть платёж в обработке»), и статус начнёт меняться.
	const credited, inProgress = money.Amount(0), false
	return invoiceResponse{
		ID:             inv.ID,
		Amount:         inv.Amount,
		AmountReceived: credited,
		Status:         invoices.Status(inv, credited, inProgress, a.now()),
		DepositAddress: inv.DepositAddress,
		ExpiresAt:      inv.ExpiresAt.UTC(),
		CreatedAt:      inv.CreatedAt.UTC(),
	}
}

// decodeCreateRequest строго разбирает тело: размер ограничен, неизвестные поля и данные
// после объекта не допускаются. Сумма принимается только JSON-строкой (см. money.Amount).
func decodeCreateRequest(w http.ResponseWriter, r *http.Request) (createInvoiceRequest, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	var req createInvoiceRequest
	if err := dec.Decode(&req); err != nil {
		return createInvoiceRequest{}, err
	}
	// После объекта допустимы только пробелы: следующий токен обязан быть концом тела.
	_, err := dec.Token()
	switch {
	case errors.Is(err, io.EOF):
		return req, nil
	case err == nil:
		return createInvoiceRequest{}, errTrailingData
	default:
		// Мусор вместо JSON или превышение размера на втором чтении.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return createInvoiceRequest{}, err
		}
		return createInvoiceRequest{}, errTrailingData
	}
}

// writeDecodeError переводит ошибку разбора тела в 400 (или 413 для слишком большого тела).
func writeDecodeError(w http.ResponseWriter, err error) {
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
		// Неизвестное поле, сумма не строкой, null, не тот тип: текст ошибки описывает вход клиента.
		writeError(w, http.StatusBadRequest, codeInvalidBody, err.Error())
	}
}
