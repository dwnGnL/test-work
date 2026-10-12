package payments

import "time"

// Паузы и размер пачки открыты тестам, чтобы они не повторяли числа.
const (
	AMLBatchSize  = amlBatchSize
	AMLFirstPause = amlFirstPause
	AMLMaxPause   = amlMaxPause
)

// SetClock подменяет часы обработчика: тесты двигают время сами и ничего не ждут.
func (p *Processor) SetClock(now func() time.Time) { p.now = now }

// SetAMLTimeout укорачивает таймаут вызова AML: тест зависшего провайдера не ждёт секунды.
func (p *Processor) SetAMLTimeout(d time.Duration) { p.amlTimeout = d }
