package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthHandler(t *testing.T) {
	tests := []struct {
		name     string
		pingErr  error
		wantCode int
		wantBody string
	}{
		{"database up", nil, http.StatusOK, `{"status":"ok"}`},
		{"database down", errors.New("connection refused"), http.StatusServiceUnavailable, `{"status":"unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("GET /healthz", healthHandler(fakePinger{tt.pingErr}, slog.New(slog.DiscardHandler)))

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}

	t.Run("only GET", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.Handle("GET /healthz", healthHandler(fakePinger{}, slog.New(slog.DiscardHandler)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d, want 405", rec.Code)
		}
	})
}
