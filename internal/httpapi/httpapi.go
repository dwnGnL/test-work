package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// API — обработчики HTTP. Создаётся через New.
type API struct {
	db         *pgxpool.Pool
	log        *slog.Logger
	invoiceTTL time.Duration    // срок жизни нового счёта (из конфигурации)
	now        func() time.Time // часы; подменяются в тестах
}

// New возвращает HTTP-обработчик со всеми маршрутами сервиса.
func New(db *pgxpool.Pool, log *slog.Logger, invoiceTTL time.Duration) http.Handler {
	a := &API{db: db, log: log, invoiceTTL: invoiceTTL, now: time.Now}
	return a.routes()
}

func (a *API) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthHandler(a.db, a.log))
	mux.Handle("POST /v1/invoices", a.auth(http.HandlerFunc(a.createInvoice)))
	mux.Handle("GET /v1/invoices/{id}", a.auth(http.HandlerFunc(a.getInvoice)))
	mux.Handle("GET /v1/balance", a.auth(http.HandlerFunc(a.getBalance)))

	// Остальное тоже отвечает JSON-ошибкой в общем формате (по умолчанию mux пишет текст):
	// известный путь с другим методом — 405, неизвестный путь — 404.
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/v1/invoices", methodNotAllowed("POST"))
	mux.HandleFunc("/v1/invoices/{id}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/v1/balance", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "not found")
	})
	return mux
}

// methodNotAllowed отвечает 405 и перечисляет разрешённые методы в заголовке Allow.
func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
	}
}

// internalError пишет в лог настоящую причину, а клиенту отвечает общим 500: текст ошибки
// базы или внутренних проверок наружу не отдаём.
func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}

// writeJSON отвечает JSON-телом с кодом status.
func (a *API) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeBody(w, status, body)
}

// writeBody пишет готовое JSON-тело. Ответы с данными партнёра (баланс, счета) не кэшируем.
// Ошибку записи не обрабатываем: она значит, что клиент уже ушёл.
func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
