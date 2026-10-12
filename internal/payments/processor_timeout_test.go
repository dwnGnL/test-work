package payments_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dwnGnL/test-work/internal/aml"
	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
)

// Зависший вызов AML по одному платежу не останавливает проход: у вызова свой таймаут, после
// него платёж остаётся ждать, а остальные платежи пачки проверяются и решаются.
func TestProcessorHungAMLDoesNotBlockTheBatch(t *testing.T) {
	e := newEnv(t)
	hung := "0x" + strings.Repeat("9", 40)
	stuck := e.seed("aa", from(hung)) // берётся первым: id меньше
	fine := e.seed("bb")

	// Провайдер «висит» на одном адресе, пока вызывающий не отменит запрос.
	checker := checkerFunc(func(ctx context.Context, address string) (aml.Verdict, error) {
		if address == hung {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return e.aml.Check(ctx, address)
	})
	p := e.processor(checker)
	p.SetAMLTimeout(50 * time.Millisecond)

	// Общий срок нужен только на случай поломки: без таймаута вызова проход не вернулся бы вовсе.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	e.assertPending(stuck)
	e.assertDecided(fine, payments.StatusCredited, aml.VerdictLow)
	e.assertBooks(map[string]money.Amount{
		ledger.AccountHoldAML:    -amt(t, "100"), // зависший платёж всё ещё в холде
		ledger.PartnerAccount(7): -amt(t, "98.5"),
	})
	if got := e.logs.String(); !containsAll(got, "AML check failed") {
		t.Errorf("log = %q, want the timed out check to be logged", got)
	}
}
