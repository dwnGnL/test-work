package invoices_test

import (
	"testing"
	"time"

	"github.com/dwnGnL/test-work/internal/invoices"
	"github.com/dwnGnL/test-work/internal/money"
)

// Status — чистая функция, базы не требует. Счёт на 100 USDT со сроком в 12:00.
func TestStatus(t *testing.T) {
	expires := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	inv := invoices.Invoice{Amount: 100 * 1_000_000, ExpiresAt: expires}
	const unit = money.Amount(1_000_000)

	before := expires.Add(-time.Minute)
	after := expires.Add(time.Minute)

	tests := []struct {
		name       string
		credited   money.Amount
		inProgress bool
		now        time.Time
		want       string
	}{
		{"nothing yet", 0, false, before, invoices.StatusPending},
		{"payment in progress, not credited yet", 0, true, before, invoices.StatusPending},
		{"part credited", 40 * unit, false, before, invoices.StatusPartiallyPaid},
		{"one unit short", 100*unit - 1, false, before, invoices.StatusPartiallyPaid},
		{"exactly the amount", 100 * unit, false, before, invoices.StatusPaid},
		{"overpaid", 150 * unit, false, before, invoices.StatusPaid},
		{"paid while another payment is in progress", 100 * unit, true, before, invoices.StatusPaid},

		{"expired, nothing paid", 0, false, after, invoices.StatusExpired},
		{"expired, payment still in progress", 0, true, after, invoices.StatusPending},
		{"expired, part credited, nothing in progress", 40 * unit, false, after, invoices.StatusExpired},
		{"expired, part credited, payment in progress", 40 * unit, true, after, invoices.StatusPartiallyPaid},
		{"paid, then the term ran out", 100 * unit, false, after, invoices.StatusPaid},
		{"paid late, with payment in progress", 100 * unit, true, after, invoices.StatusPaid},

		// Граница срока: ровно в ExpiresAt счёт ещё живой, expired наступает строго после.
		{"at the very expiry time", 0, false, expires, invoices.StatusPending},
		{"a nanosecond after the expiry time", 0, false, expires.Add(time.Nanosecond), invoices.StatusExpired},
		{"a nanosecond before the expiry time", 0, false, expires.Add(-time.Nanosecond), invoices.StatusPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := invoices.Status(inv, tt.credited, tt.inProgress, tt.now); got != tt.want {
				t.Errorf("Status(credited=%s, inProgress=%v, now=%s) = %q, want %q",
					tt.credited, tt.inProgress, tt.now.Format(time.RFC3339Nano), got, tt.want)
			}
		})
	}
}

// Статус сравнивает со счётом сумму до комиссии. Счёт на 100 USDT, ставка 1,5%: клиент прислал
// 100, партнёру зачислено 98,5. Счёт оплачен; если бы сюда передали 98,5, он навсегда остался бы
// «частично оплаченным».
func TestStatusUsesAmountBeforeFee(t *testing.T) {
	expires := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	inv := invoices.Invoice{Amount: 100_000_000, FeeBps: 150, ExpiresAt: expires}
	now := expires.Add(-time.Minute)

	fee, err := money.FeeBps(inv.Amount, inv.FeeBps)
	if err != nil {
		t.Fatal(err)
	}
	if got := invoices.Status(inv, inv.Amount, false, now); got != invoices.StatusPaid {
		t.Errorf("received the full amount: status = %s, want paid", got)
	}
	if got := invoices.Status(inv, inv.Amount-fee, false, now); got != invoices.StatusPartiallyPaid {
		t.Errorf("amount after fee passed by mistake: status = %s, want partially_paid", got)
	}
}
