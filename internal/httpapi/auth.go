package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dwnGnL/test-work/internal/partners"
)

// partnerKey — ключ значения «партнёр» в контексте запроса. Тип свой, чтобы не пересекаться
// с ключами других пакетов.
type partnerKey struct{}

// auth пускает дальше только запросы с верным API-ключом в заголовке
// Authorization: Bearer <ключ> и кладёт найденного партнёра в контекст.
// Нет ключа и неверный ключ неотличимы: оба дают 401.
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partner, err := partners.Authenticate(r.Context(), a.db, bearerToken(r))
		if errors.Is(err, partners.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid API key")
			return
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), partnerKey{}, partner)))
	})
}

// partnerFrom достаёт партнёра, которого положил auth. Его нет, только если обработчик
// зарегистрирован без auth: это ошибка сборки маршрутов, наружу она уходит как 500.
func (a *API) partnerFrom(w http.ResponseWriter, r *http.Request) (partners.Partner, bool) {
	p, ok := r.Context().Value(partnerKey{}).(partners.Partner)
	if !ok {
		a.internalError(w, r, errors.New("httpapi: handler runs without authentication"))
	}
	return p, ok
}

// bearerToken возвращает ключ из заголовка "Authorization: Bearer <ключ>" или пустую строку.
// Название схемы по RFC 7235 не зависит от регистра.
func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
