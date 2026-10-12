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
