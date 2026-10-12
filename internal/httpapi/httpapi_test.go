package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/partners"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Интеграционные тесты на настоящей базе: нужен TEST_DATABASE_URL, иначе пропускаются (см. pgtest).

const (
	keyAlpha  = "test-key-alpha"
	keyBeta   = "test-key-beta"
	testTTL   = 90 * time.Minute
	notFound  = `{"error":{"code":"not_found","message":"invoice not found"}}`
	jsonType  = "application/json"
	validBody = `{"amount":"100.000000"}`
)

var (
	uuidRe    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	addressRe = regexp.MustCompile(`^0x[0-9a-f]{40}$`)
)

// fixture — база с двумя партнёрами (alpha: 1,5%, beta: 0,5%) и обработчик поверх неё.
type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	api   *API
	h     http.Handler
	logs  *bytes.Buffer
	alpha partners.Partner
	beta  partners.Partner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewMigrated(t)
	logs := &bytes.Buffer{}
	api := &API{db: pool, log: slog.New(slog.NewTextHandler(logs, nil)), invoiceTTL: testTTL, now: time.Now}
	f := &fixture{t: t, pool: pool, api: api, h: api.routes(), logs: logs}
	f.alpha = f.addPartner("alpha", keyAlpha, 150)
	f.beta = f.addPartner("beta", keyBeta, 50)
	return f
}

func (f *fixture) addPartner(name, key string, bps int) partners.Partner {
	f.t.Helper()
	var p partners.Partner
	err := postgres.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		p, err = partners.Create(context.Background(), tx, name, key, bps)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// do отправляет запрос. apiKey пустой — без входа; headers — пары «имя, значение».
func (f *fixture) do(method, path, apiKey, body string, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// createInvoice создаёт счёт с ключом идемпотентности key.
func (f *fixture) createInvoice(apiKey, key, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPost, "/v1/invoices", apiKey, body, "Idempotency-Key", key)
}

// invoiceRow — ответ API по счёту в виде, удобном для проверок.
type invoiceRow struct {
	ID             string `json:"id"`
	Amount         string `json:"amount"`
	AmountReceived string `json:"amount_received"`
	Status         string `json:"status"`
	DepositAddress string `json:"deposit_address"`
	ExpiresAt      string `json:"expires_at"`
	CreatedAt      string `json:"created_at"`
}

type errorRow struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not the expected JSON: %v\nstatus %d, body: %s", err, rec.Code, rec.Body)
	}
	return v
}

// wantStatus проверяет код ответа и тип содержимого.
func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonType)
	}
}

// wantError проверяет ответ-ошибку в едином формате.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	wantStatus(t, rec, status)
	if got := decode[errorRow](t, rec).Error; got.Code != code || got.Message == "" {
		t.Errorf("error = %+v, want code %q and a message", got, code)
	}
}

func (f *fixture) countInvoices() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices.invoices`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestCreateInvoice(t *testing.T) {
	f := newFixture(t)
	before := time.Now()

	rec := f.createInvoice(keyAlpha, "order-1", validBody)
	wantStatus(t, rec, http.StatusCreated)
	inv := decode[invoiceRow](t, rec)

	if !uuidRe.MatchString(inv.ID) {
		t.Errorf("id = %q, want a UUID", inv.ID)
	}
	if inv.Amount != "100.000000" || inv.AmountReceived != "0.000000" || inv.Status != "pending" {
		t.Errorf("amount = %q, amount_received = %q, status = %q", inv.Amount, inv.AmountReceived, inv.Status)
	}
	if !addressRe.MatchString(inv.DepositAddress) {
		t.Errorf("deposit_address = %q, want 0x and 40 lowercase hex digits", inv.DepositAddress)
	}

	// Времена: RFC 3339 в UTC, срок — ровно TTL из конфигурации.
	for name, s := range map[string]string{"expires_at": inv.ExpiresAt, "created_at": inv.CreatedAt} {
		if !strings.HasSuffix(s, "Z") {
			t.Errorf("%s = %q, want UTC (suffix Z)", name, s)
		}
	}
	created, err1 := time.Parse(time.RFC3339, inv.CreatedAt)
	expires, err2 := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err1 != nil || err2 != nil {
		t.Fatalf("times are not RFC 3339: %v, %v", err1, err2)
	}
	if expires.Sub(created) != testTTL {
		t.Errorf("expires_at - created_at = %s, want %s", expires.Sub(created), testTTL)
	}
	if created.Before(before.Add(-time.Minute)) || created.After(time.Now().Add(time.Minute)) {
		t.Errorf("created_at = %s is far from now", created)
	}

	// Ставка в счёт копируется у партнёра (alpha — 150 б.п.), в запросе её нет.
	var feeBps, partnerID int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT fee_bps, partner_id FROM invoices.invoices WHERE id = $1`, inv.ID).Scan(&feeBps, &partnerID); err != nil {
		t.Fatal(err)
	}
	if feeBps != 150 || int64(partnerID) != f.alpha.ID {
		t.Errorf("stored fee_bps = %d, partner_id = %d; want 150 and %d", feeBps, partnerID, f.alpha.ID)
	}

	// Тот же счёт виден по GET.
	got := f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, "")
	wantStatus(t, got, http.StatusOK)
	if again := decode[invoiceRow](t, got); again != inv {
		t.Errorf("GET returned %+v, want %+v", again, inv)
	}
}

// Ставка у каждого партнёра своя.
func TestCreateInvoiceUsesPartnerFee(t *testing.T) {
	f := newFixture(t)
	for key, want := range map[string]int{keyAlpha: 150, keyBeta: 50} {
		inv := decode[invoiceRow](t, f.createInvoice(key, "order-1", validBody))
		var got int
		if err := f.pool.QueryRow(context.Background(),
			`SELECT fee_bps FROM invoices.invoices WHERE id = $1`, inv.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("partner %s: fee_bps = %d, want %d", key, got, want)
		}
	}
}

// Повтор с тем же ключом и тем же телом — 200 и тот же счёт. «Тот же» тело — это тот же запрос,
// а не те же байты: пробелы, порядок полей и запись суммы значения не имеют.
func TestCreateInvoiceRepeat(t *testing.T) {
	f := newFixture(t)

	first := f.createInvoice(keyAlpha, "order-1", validBody)
	wantStatus(t, first, http.StatusCreated)
	want := decode[invoiceRow](t, first)

	for _, body := range []string{
		validBody,
		"{ \"amount\" :\n\t\"100.000000\" }\n",
		`{"amount":"100"}`,
		`{"amount":"0100.0"}`,
	} {
		rec := f.createInvoice(keyAlpha, "order-1", body)
		wantStatus(t, rec, http.StatusOK)
		if got := decode[invoiceRow](t, rec); got != want {
			t.Errorf("body %q: got %+v, want the original %+v", body, got, want)
		}
	}
	if n := f.countInvoices(); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}
}

func TestCreateInvoiceConflict(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.createInvoice(keyAlpha, "order-1", validBody), http.StatusCreated)

	rec := f.createInvoice(keyAlpha, "order-1", `{"amount":"100.000001"}`)
	wantError(t, rec, http.StatusUnprocessableEntity, "idempotency_conflict")
	if n := f.countInvoices(); n != 1 {
		t.Errorf("invoices in the table = %d, want 1", n)
	}

	// У другого партнёра тот же ключ — его собственный счёт.
	rec = f.createInvoice(keyBeta, "order-1", `{"amount":"100.000001"}`)
	wantStatus(t, rec, http.StatusCreated)
	if n := f.countInvoices(); n != 2 {
		t.Errorf("invoices in the table = %d, want 2", n)
	}
}

func TestCreateInvoiceIdempotencyKey(t *testing.T) {
	f := newFixture(t)

	t.Run("missing", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/v1/invoices", keyAlpha, validBody)
		wantError(t, rec, http.StatusBadRequest, "idempotency_key_required")
	})
	t.Run("empty", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/v1/invoices", keyAlpha, validBody, "Idempotency-Key", "")
		wantError(t, rec, http.StatusBadRequest, "idempotency_key_required")
	})
	t.Run("too long", func(t *testing.T) {
		rec := f.createInvoice(keyAlpha, strings.Repeat("k", 256), validBody)
		wantError(t, rec, http.StatusBadRequest, "invalid_idempotency_key")
	})
	t.Run("longest allowed", func(t *testing.T) {
		wantStatus(t, f.createInvoice(keyAlpha, strings.Repeat("k", 255), validBody), http.StatusCreated)
	})
	t.Run("not utf-8", func(t *testing.T) {
		rec := f.createInvoice(keyAlpha, "bad\xff\xfekey", validBody)
		wantError(t, rec, http.StatusBadRequest, "invalid_idempotency_key")
	})
}

// Неверная сумма и мусор в теле — 400, счёт не создаётся.
func TestCreateInvoiceBadBody(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name string
		body string
		code string
	}{
		{"amount as integer", `{"amount":100}`, "invalid_body"},
		{"amount as float", `{"amount":100.5}`, "invalid_body"},
		{"amount null", `{"amount":null}`, "invalid_body"},
		{"amount as object", `{"amount":{"v":"1"}}`, "invalid_body"},
		{"seven decimal places", `{"amount":"1.0000001"}`, "invalid_amount"},
		{"zero", `{"amount":"0"}`, "invalid_amount"},
		{"zero with decimals", `{"amount":"0.000000"}`, "invalid_amount"},
		{"negative", `{"amount":"-1"}`, "invalid_amount"},
		{"empty string", `{"amount":""}`, "invalid_amount"},
		{"letters", `{"amount":"abc"}`, "invalid_amount"},
		{"exponent", `{"amount":"1e3"}`, "invalid_amount"},
		{"leading space", `{"amount":" 1"}`, "invalid_amount"},
		{"comma", `{"amount":"1,5"}`, "invalid_amount"},
		{"no integer part", `{"amount":".5"}`, "invalid_amount"},
		{"overflow", `{"amount":"9223372036854.775808"}`, "invalid_amount"},
		{"no amount", `{}`, "invalid_amount"},
		{"unknown field", `{"amount":"1","fee_bps":0}`, "invalid_body"},
		{"garbage after the object", `{"amount":"1"} garbage`, "invalid_body"},
		{"second object", `{"amount":"1"}{"amount":"2"}`, "invalid_body"},
		{"stray brace", `{"amount":"1"}}`, "invalid_body"},
		{"empty body", ``, "invalid_body"},
		{"whitespace only", " \n", "invalid_body"},
		{"not json", `amount=1`, "invalid_body"},
		{"truncated", `{"amount":"1"`, "invalid_body"},
		{"array", `[]`, "invalid_body"},
		{"bare string", `"100.000000"`, "invalid_body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.createInvoice(keyAlpha, "order-1", tt.body), http.StatusBadRequest, tt.code)
		})
	}
	if n := f.countInvoices(); n != 0 {
		t.Errorf("invoices in the table = %d, want 0: rejected requests must not create a record", n)
	}

	// Пробелы после объекта — не мусор.
	wantStatus(t, f.createInvoice(keyAlpha, "order-1", `{"amount":"1"}`+" \n\t "), http.StatusCreated)
}

// Тело больше предела отклоняется: и когда лишнее внутри объекта, и когда после него.
func TestCreateInvoiceBodyTooLarge(t *testing.T) {
	f := newFixture(t)

	huge := `{"amount":"1","note":"` + strings.Repeat("x", 4*maxBodyBytes) + `"}`
	wantError(t, f.createInvoice(keyAlpha, "order-1", huge), http.StatusRequestEntityTooLarge, "body_too_large")

	padded := `{"amount":"1"}` + strings.Repeat(" ", 4*maxBodyBytes)
	wantError(t, f.createInvoice(keyAlpha, "order-2", padded), http.StatusRequestEntityTooLarge, "body_too_large")

	if n := f.countInvoices(); n != 0 {
		t.Errorf("invoices in the table = %d, want 0", n)
	}
}

// Без входа и с неверным ключом — 401 на всех ручках, кроме /healthz.
func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	endpoints := []struct{ name, method, path string }{
		{"create invoice", http.MethodPost, "/v1/invoices"},
		{"get invoice", http.MethodGet, "/v1/invoices/" + inv.ID},
		{"balance", http.MethodGet, "/v1/balance"},
	}
	authorizations := map[string]string{
		"no header":              "",
		"empty bearer":           "Bearer ",
		"bearer without a space": "Bearer",
		"unknown key":            "Bearer nope",
		"key with a suffix":      "Bearer " + keyAlpha + "x",
		"other case of the key":  "Bearer " + strings.ToUpper(keyAlpha),
		"other scheme":           "Basic " + keyAlpha,
		"key without a scheme":   keyAlpha,
	}
	for name, authorization := range authorizations {
		for _, ep := range endpoints {
			t.Run(name+"/"+ep.name, func(t *testing.T) {
				// Idempotency-Key и тело верные: ответ определяет только вход.
				rec := f.do(ep.method, ep.path, "", validBody, "Authorization", authorization, "Idempotency-Key", "k")
				wantError(t, rec, http.StatusUnauthorized, "unauthorized")
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want Bearer", got)
				}
			})
		}
	}

	// Вход проверяется раньше, чем заголовок и тело: без ключа это 401, а не 400.
	wantError(t, f.do(http.MethodPost, "/v1/invoices", "", `junk`), http.StatusUnauthorized, "unauthorized")

	// Название схемы не зависит от регистра (RFC 7235).
	rec := f.do(http.MethodGet, "/v1/balance", "", "", "Authorization", "bearer "+keyAlpha)
	wantStatus(t, rec, http.StatusOK)

	// /healthz открыт без входа.
	rec = f.do(http.MethodGet, "/healthz", "", "")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != `{"status":"ok"}` {
		t.Errorf("healthz body = %s", rec.Body)
	}
}

// Партнёр не видит чужие счета: для него чужой счёт неотличим от несуществующего.
func TestInvoiceIsolation(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))

	wantStatus(t, f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, ""), http.StatusOK)

	foreign := f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyBeta, "")
	wantError(t, foreign, http.StatusNotFound, "not_found")

	// Тот же ответ, байт в байт, что и на несуществующий и на неверный id.
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "nope", "1", inv.ID + "0"} {
		rec := f.do(http.MethodGet, "/v1/invoices/"+id, keyBeta, "")
		wantStatus(t, rec, http.StatusNotFound)
		if rec.Body.String() != foreign.Body.String() {
			t.Errorf("id %q: body %s differs from the foreign-invoice body %s", id, rec.Body, foreign.Body)
		}
	}
	if foreign.Body.String() != notFound {
		t.Errorf("body = %s, want %s", foreign.Body, notFound)
	}

	// Чужой счёт не мешает создать свой с тем же ключом идемпотентности.
	other := f.createInvoice(keyBeta, "order-1", validBody)
	wantStatus(t, other, http.StatusCreated)
	if decode[invoiceRow](t, other).ID == inv.ID {
		t.Error("partners share an invoice")
	}
}

func TestBalance(t *testing.T) {
	f := newFixture(t)

	t.Run("new partner has zero", func(t *testing.T) {
		for _, key := range []string{keyAlpha, keyBeta} {
			rec := f.do(http.MethodGet, "/v1/balance", key, "")
			wantStatus(t, rec, http.StatusOK)
			if rec.Body.String() != `{"balance":"0.000000"}` {
				t.Errorf("balance = %s, want 0.000000", rec.Body)
			}
		}
	})

	t.Run("each partner sees only their own", func(t *testing.T) {
		// Зачисляем alpha 98.5 USDT, как это сделает обработчик платежей.
		ctx := context.Background()
		const hundred, net, fee = money.Amount(100_000_000), money.Amount(98_500_000), money.Amount(1_500_000)
		err := postgres.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			for _, p := range []ledger.Posting{
				{Key: "p1:1", Kind: "hold", Lines: []ledger.Line{
					ledger.Debit(ledger.AccountDeposits, hundred),
					ledger.Credit(ledger.AccountHoldAML, hundred),
				}},
				{Key: "p1:2", Kind: "credit", Lines: []ledger.Line{
					ledger.Debit(ledger.AccountHoldAML, hundred),
					ledger.Credit(ledger.PartnerAccount(f.alpha.ID), net),
					ledger.Credit(ledger.AccountRevenueFees, fee),
				}},
			} {
				if _, err := ledger.Post(ctx, tx, p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		if rec := f.do(http.MethodGet, "/v1/balance", keyAlpha, ""); rec.Body.String() != `{"balance":"98.500000"}` {
			t.Errorf("alpha balance = %s, want 98.500000", rec.Body)
		}
		if rec := f.do(http.MethodGet, "/v1/balance", keyBeta, ""); rec.Body.String() != `{"balance":"0.000000"}` {
			t.Errorf("beta balance = %s, want 0.000000", rec.Body)
		}
	})
}

// Статус счёта выводится через invoices.Status; платежей пока нет, поэтому после срока счёт expired.
func TestInvoiceStatusFollowsTime(t *testing.T) {
	f := newFixture(t)
	inv := decode[invoiceRow](t, f.createInvoice(keyAlpha, "order-1", validBody))
	expires, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"before the expiry", expires.Add(-time.Minute), "pending"},
		{"after the expiry", expires.Add(time.Minute), "expired"},
	} {
		f.api.now = func() time.Time { return tt.now }
		rec := f.do(http.MethodGet, "/v1/invoices/"+inv.ID, keyAlpha, "")
		wantStatus(t, rec, http.StatusOK)
		if got := decode[invoiceRow](t, rec).Status; got != tt.want {
			t.Errorf("%s: status = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Внутренняя ошибка: клиент получает общий 500, подробности — в логе.
func TestInternalErrorsAreNotExposed(t *testing.T) {
	f := newFixture(t)
	f.pool.Close() // база «пропала»: вход упадёт на обращении к ней

	rec := f.do(http.MethodGet, "/v1/balance", keyAlpha, "")
	wantError(t, rec, http.StatusInternalServerError, "internal_error")
	if got := decode[errorRow](t, rec).Error.Message; got != "internal error" {
		t.Errorf("message = %q, want the generic text", got)
	}
	for _, leak := range []string{"pool", "closed", "partners", "SELECT", "sql"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(leak)) {
			t.Errorf("response leaks %q: %s", leak, rec.Body)
		}
	}
	if !strings.Contains(f.logs.String(), "request failed") || !strings.Contains(f.logs.String(), "closed pool") {
		t.Errorf("the real cause is missing from the log: %s", f.logs)
	}
	if strings.Contains(f.logs.String(), keyAlpha) {
		t.Errorf("the API key got into the log: %s", f.logs)
	}
}

// Неизвестный путь и неверный метод тоже отвечают JSON-ошибкой в общем формате.
func TestUnknownRoutes(t *testing.T) {
	f := newFixture(t)

	wantError(t, f.do(http.MethodGet, "/nope", keyAlpha, ""), http.StatusNotFound, "not_found")
	wantError(t, f.do(http.MethodGet, "/v1/invoices", keyAlpha, ""), http.StatusMethodNotAllowed, "method_not_allowed")
	wantError(t, f.do(http.MethodPost, "/v1/balance", keyAlpha, ""), http.StatusMethodNotAllowed, "method_not_allowed")
	wantError(t, f.do(http.MethodDelete, "/v1/invoices/"+strings.Repeat("0", 8), keyAlpha, ""), http.StatusMethodNotAllowed, "method_not_allowed")
	rec := f.do(http.MethodPost, "/healthz", "", "")
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Errorf("Allow = %q", got)
	}
}
