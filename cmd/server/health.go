package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// pinger — то, что умеет проверять доступность базы (*pgxpool.Pool).
type pinger interface {
	Ping(ctx context.Context) error
}

// healthTimeout ограничивает проверку базы: зависшая база не должна вешать пробу.
const healthTimeout = 2 * time.Second

// healthHandler отвечает 200, если база доступна, и 503 иначе.
// Причину сбоя пишем в лог, а не в ответ: проба открыта наружу.
func healthHandler(db pinger, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := db.Ping(ctx); err != nil {
			log.Error("healthz: database ping failed", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}
