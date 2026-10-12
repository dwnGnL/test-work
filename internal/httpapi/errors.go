package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Коды ошибок в ответе: клиент опирается на них, а не на текст сообщения.
const (
	codeUnauthorized           = "unauthorized"
	codeIdempotencyKeyRequired = "idempotency_key_required"
	codeInvalidIdempotencyKey  = "invalid_idempotency_key"
	codeInvalidBody            = "invalid_body"
	codeInvalidAmount          = "invalid_amount"
	codeBodyTooLarge           = "body_too_large"
	codeIdempotencyConflict    = "idempotency_conflict"
	codeNotFound               = "not_found"
	codeMethodNotAllowed       = "method_not_allowed"
	codeInternal               = "internal_error"
)

var (
	// errTrailingData — после JSON-объекта в теле есть что-то ещё.
	errTrailingData = errors.New("unexpected data after the JSON object")
	// errNoPartner — обработчик вызван без auth: в контексте запроса нет партнёра.
	// Это ошибка сборки маршрутов, наружу она уходит как 500.
	errNoPartner = errors.New("httpapi: handler runs without authentication")
)

// errorResponse — единый формат ошибки: {"error":{"code":"...","message":"..."}}.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError отвечает ошибкой в едином формате. Сообщение должно быть безопасным для клиента:
// внутренние ошибки сюда не передают, их подробности идут в лог (см. API.internalError).
func writeError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(errorResponse{Error: errorDetail{Code: code, Message: message}})
	if err != nil {
		// Две строки не кодировать нельзя; на всякий случай отвечаем без тела.
		w.WriteHeader(status)
		return
	}
	writeBody(w, status, body)
}
