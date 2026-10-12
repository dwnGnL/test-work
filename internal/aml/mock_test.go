package aml_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dwnGnL/test-work/internal/aml"
)

// Тесты без базы: у модуля нет состояния.

const (
	dirty = "0x000000000000000000000000000000000000dead"
	clean = "0x1111111111111111111111111111111111111111"
)

// Мок подходит под интерфейс, которым пользуется обработчик платежей.
var _ aml.Checker = (*aml.Mock)(nil)

func TestMockVerdicts(t *testing.T) {
	tests := []struct {
		name      string
		blacklist []string
		address   string
		want      aml.Verdict
	}{
		{"blacklisted address", []string{dirty}, dirty, aml.VerdictHigh},
		{"other address", []string{dirty}, clean, aml.VerdictLow},
		{"address in upper case, list in lower case", []string{dirty}, "0x000000000000000000000000000000000000DEAD", aml.VerdictHigh},
		{"address in lower case, list in upper case", []string{"0x000000000000000000000000000000000000DEAD"}, dirty, aml.VerdictHigh},
		{"second entry of the list", []string{clean, dirty}, dirty, aml.VerdictHigh},
		{"empty list", nil, dirty, aml.VerdictLow},
		{"empty address", []string{dirty}, "", aml.VerdictLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := aml.NewMock(tt.blacklist).Check(context.Background(), tt.address)
			if err != nil || got != tt.want {
				t.Errorf("Check(%q) = %q, %v; want %q", tt.address, got, err, tt.want)
			}
		})
	}
}

// Ответ детерминирован: один и тот же адрес всегда даёт один и тот же вердикт.
func TestMockIsDeterministic(t *testing.T) {
	m := aml.NewMock([]string{dirty})
	for range 5 {
		if got, _ := m.Check(context.Background(), dirty); got != aml.VerdictHigh {
			t.Fatalf("Check(dirty) = %q, want high", got)
		}
		if got, _ := m.Check(context.Background(), clean); got != aml.VerdictLow {
			t.Fatalf("Check(clean) = %q, want low", got)
		}
	}
}

// Выключенный мок отвечает ошибкой, а не вердиктом, для любого адреса; включённый снова работает.
func TestMockUnavailable(t *testing.T) {
	m := aml.NewMock([]string{dirty})
	m.SetAvailable(false)

	for _, address := range []string{dirty, clean} {
		got, err := m.Check(context.Background(), address)
		if !errors.Is(err, aml.ErrUnavailable) || got != "" {
			t.Errorf("Check(%q) while unavailable = %q, %v; want no verdict and ErrUnavailable", address, got, err)
		}
	}

	m.SetAvailable(true)
	if got, err := m.Check(context.Background(), dirty); err != nil || got != aml.VerdictHigh {
		t.Errorf("Check after SetAvailable(true) = %q, %v; want high", got, err)
	}
}

// Отменённый контекст не даёт вердикта.
func TestMockCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := aml.NewMock(nil).Check(ctx, clean); !errors.Is(err, context.Canceled) || got != "" {
		t.Errorf("Check with a canceled context = %q, %v; want no verdict and context.Canceled", got, err)
	}
}

// Список копируется при создании: правка исходного среза мок не меняет.
func TestMockCopiesBlacklist(t *testing.T) {
	list := []string{dirty}
	m := aml.NewMock(list)
	list[0] = clean

	if got, _ := m.Check(context.Background(), dirty); got != aml.VerdictHigh {
		t.Errorf("Check(dirty) = %q after the caller changed its slice, want high", got)
	}
	if got, _ := m.Check(context.Background(), clean); got != aml.VerdictLow {
		t.Errorf("Check(clean) = %q after the caller changed its slice, want low", got)
	}
}

// Параллельные Check и SetAvailable безопасны (проверяет детектор гонок): каждый ответ —
// либо верный вердикт, либо ErrUnavailable, и никогда неверный вердикт.
func TestMockConcurrent(t *testing.T) {
	m := aml.NewMock([]string{dirty})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				if i%4 == 0 {
					m.SetAvailable(j%2 == 0)
					continue
				}
				got, err := m.Check(context.Background(), dirty)
				if err != nil && !errors.Is(err, aml.ErrUnavailable) {
					t.Errorf("Check: unexpected error %v", err)
				}
				if err == nil && got != aml.VerdictHigh {
					t.Errorf("Check(dirty) = %q, want high", got)
				}
			}
		}()
	}
	wg.Wait()
}
