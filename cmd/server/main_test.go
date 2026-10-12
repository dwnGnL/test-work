package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// stubScanner — сканер-заглушка: на проходе вызывает tick.
type stubScanner struct{ tick func() error }

func (s stubScanner) Tick(context.Context) error { return s.tick() }

// Проход не роняет сервис ни паникой, ни ошибкой: оба случая попадают в лог уровня Error.
// Отмена контекста при остановке и успешный проход в лог не пишут.
func TestScanOnce(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tt := range []struct {
		name    string
		ctx     context.Context
		tick    func() error
		wantLog []string // пусто — в логе ничего нет
	}{
		{"panic", context.Background(), func() error { panic("library bug") },
			[]string{"level=ERROR", "scanner tick panicked", "library bug", "goroutine"}},
		{"error", context.Background(), func() error { return errors.New("db is down") },
			[]string{"level=ERROR", "scanner tick failed", "db is down"}},
		{"error on shutdown", canceled, func() error { return context.Canceled }, nil},
		{"success", context.Background(), func() error { return nil }, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))

			scanOnce(tt.ctx, stubScanner{tt.tick}, log) // паника наружу не выходит: иначе упадёт сам тест

			if len(tt.wantLog) == 0 && logs.Len() != 0 {
				t.Errorf("log = %q, want nothing", logs.String())
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log = %q, want it to contain %q", logs.String(), want)
				}
			}
		})
	}
}
