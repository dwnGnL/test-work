package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// stubTicker — обработчик-заглушка: на проходе вызывает tick.
type stubTicker struct{ tick func() error }

func (s stubTicker) Tick(context.Context) error { return s.tick() }

// Проход не роняет сервис ни паникой, ни ошибкой: оба случая попадают в лог уровня Error вместе
// с именем цикла. Отмена контекста при остановке и успешный проход в лог не пишут.
func TestTickOnce(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tt := range []struct {
		name    string
		worker  string // имя цикла, как его передаёт run
		ctx     context.Context
		tick    func() error
		wantLog []string // пусто — в логе ничего нет
	}{
		{"panic", "scanner", context.Background(), func() error { panic("library bug") },
			[]string{"level=ERROR", "scanner tick panicked", "library bug", "goroutine"}},
		{"error", "scanner", context.Background(), func() error { return errors.New("db is down") },
			[]string{"level=ERROR", "scanner tick failed", "db is down"}},
		{"payment processor panic", "payment processor", context.Background(), func() error { panic("library bug") },
			[]string{"level=ERROR", "payment processor tick panicked", "library bug", "goroutine"}},
		{"payment processor error", "payment processor", context.Background(), func() error { return errors.New("db is down") },
			[]string{"level=ERROR", "payment processor tick failed", "db is down"}},
		{"error on shutdown", "scanner", canceled, func() error { return context.Canceled }, nil},
		{"success", "scanner", context.Background(), func() error { return nil }, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))

			tickOnce(tt.ctx, tt.worker, stubTicker{tt.tick}, log) // паника наружу не выходит: иначе упадёт сам тест

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
