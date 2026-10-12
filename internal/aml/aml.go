package aml

import (
	"context"
	"strings"
	"sync/atomic"
)

// Verdict — вердикт AML-проверки. Значения те же, что разрешает CHECK в миграции 0010.
type Verdict string

// Вердикты. Третьего исхода (средний риск, ручной разбор) в коде нет, он описан в документе.
const (
	VerdictLow  Verdict = "low"  // риск низкий: платёж можно зачислить
	VerdictHigh Verdict = "high" // риск высокий: платёж не зачисляется, создаётся возврат
)

// Checker — проверка адреса отправителя. Интерфейс нужен по смыслу: в проекте мок, а настоящий
// провайдер — внешний HTTP-сервис, который подключается без изменений в обработчике платежей.
//
// Ошибка (например, ErrUnavailable) — не вердикт: вызывающий не должен ни зачислять, ни
// отклонять платёж. Вызывать Check нужно вне транзакции базы: внешний ответ может прийти не сразу.
type Checker interface {
	Check(ctx context.Context, address string) (Verdict, error)
}

// Mock — детерминированный AML: адрес из чёрного списка — высокий риск, остальные — низкий.
// Умеет «падать» (SetAvailable), чтобы проверять недоступность провайдера. Безопасен для
// параллельного использования: чёрный список после создания не меняется, а выключатель атомарный.
type Mock struct {
	blacklist map[string]struct{} // адреса в нижнем регистре
	available atomic.Bool
}

// NewMock возвращает мок с чёрным списком; регистр адресов не важен. Изначально мок доступен.
func NewMock(blacklist []string) *Mock {
	m := &Mock{blacklist: make(map[string]struct{}, len(blacklist))}
	for _, address := range blacklist {
		m.blacklist[strings.ToLower(address)] = struct{}{}
	}
	m.available.Store(true)
	return m
}

// SetAvailable включает и выключает мок. Выключенный мок отвечает на Check ошибкой ErrUnavailable.
func (m *Mock) SetAvailable(available bool) {
	m.available.Store(available)
}

// Check возвращает VerdictHigh для адреса из чёрного списка и VerdictLow для остальных.
// Если мок выключен — ErrUnavailable. Отменённый контекст — его ошибка, как у настоящего клиента.
func (m *Mock) Check(ctx context.Context, address string) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !m.available.Load() {
		return "", ErrUnavailable
	}
	if _, found := m.blacklist[strings.ToLower(address)]; found {
		return VerdictHigh, nil
	}
	return VerdictLow, nil
}
