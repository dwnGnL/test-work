package payments_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/money"
	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// seed создаёт платёж счёта invoice на amount USDT в нужном статусе (блок 5, блока нет у orphaned).
func seed(t *testing.T, pool *pgxpool.Pool, invoice, tx, amount string, status payments.Status) {
	t.Helper()
	p := params(t, tx, 0, 5)
	p.InvoiceID = invoice
	p.Amount = amt(t, amount)
	mustDetect(t, pool, p)
	if status != payments.StatusDetected {
		setStatus(t, pool, find(t, pool, "0x"+tx, 0).ID, status)
	}
	if status == payments.StatusOrphaned {
		if _, err := pool.Exec(context.Background(),
			`UPDATE payments.payments SET block_number = NULL, block_hash = NULL WHERE id = $1`,
			find(t, pool, "0x"+tx, 0).ID); err != nil {
			t.Fatal(err)
		}
	}
}

func summary(t *testing.T, pool *pgxpool.Pool, invoice string) (money.Amount, bool) {
	t.Helper()
	received, inProgress, err := payments.Summary(context.Background(), pool, invoice)
	if err != nil {
		t.Fatalf("Summary(%s): %v", invoice, err)
	}
	return received, inProgress
}

// Summary суммирует только credited (поле amount, до комиссии) и только своего счёта.
func TestSummaryCountsOnlyCreditedOfOwnInvoice(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	seed(t, pool, invoiceA, "c1", "10", payments.StatusCredited)
	seed(t, pool, invoiceA, "c2", "5.5", payments.StatusCredited)
	seed(t, pool, invoiceA, "d1", "3", payments.StatusDetected)
	seed(t, pool, invoiceA, "p1", "2", payments.StatusAMLPending)
	seed(t, pool, invoiceA, "o1", "1", payments.StatusOrphaned)
	seed(t, pool, invoiceA, "r1", "7", payments.StatusRejected)
	seed(t, pool, invoiceB, "c3", "100", payments.StatusCredited)

	if received, inProgress := summary(t, pool, invoiceA); received != amt(t, "15.5") || !inProgress {
		t.Errorf("Summary(A) = %s, %v; want 15.500000 and in progress", received, inProgress)
	}
	if received, inProgress := summary(t, pool, invoiceB); received != amt(t, "100") || inProgress {
		t.Errorf("Summary(B) = %s, %v; want 100.000000 and nothing in progress", received, inProgress)
	}
}

// «В обработке» — только detected и aml_pending; счёт без платежей — ноль и false.
func TestSummaryInProgress(t *testing.T) {
	for _, tt := range []struct {
		status     payments.Status
		inProgress bool
	}{
		{payments.StatusDetected, true},
		{payments.StatusAMLPending, true},
		{payments.StatusOrphaned, false},
		{payments.StatusCredited, false},
		{payments.StatusRejected, false},
	} {
		t.Run(string(tt.status), func(t *testing.T) {
			pool := pgtest.NewMigrated(t)
			seed(t, pool, invoiceA, "aa", "10", tt.status)
			received, inProgress := summary(t, pool, invoiceA)
			wantReceived := money.Amount(0)
			if tt.status == payments.StatusCredited {
				wantReceived = amt(t, "10")
			}
			if received != wantReceived || inProgress != tt.inProgress {
				t.Errorf("Summary = %s, %v; want %s, %v", received, inProgress, wantReceived, tt.inProgress)
			}
		})
	}

	pool := pgtest.NewMigrated(t)
	if received, inProgress := summary(t, pool, invoiceA); received != 0 || inProgress {
		t.Errorf("Summary of an invoice without payments = %s, %v; want 0 and false", received, inProgress)
	}
}

// ListByInvoice отдаёт платежи только своего счёта в порядке создания, включая orphaned без блока.
func TestListByInvoice(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	seed(t, pool, invoiceA, "aa", "10", payments.StatusDetected)
	seed(t, pool, invoiceB, "bb", "99", payments.StatusDetected)
	seed(t, pool, invoiceA, "cc", "20", payments.StatusOrphaned)

	list, err := payments.ListByInvoice(context.Background(), pool, invoiceA)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByInvoice = %+v, want two payments", list)
	}
	first, second := list[0], list[1]
	if first.TxHash != "0xaa" || first.Status != payments.StatusDetected || first.Amount != amt(t, "10") ||
		first.BlockNumber == nil || *first.BlockNumber != 5 {
		t.Errorf("first = %+v", first)
	}
	if second.TxHash != "0xcc" || second.Status != payments.StatusOrphaned || second.Amount != amt(t, "20") ||
		second.BlockNumber != nil || second.BlockHash != nil {
		t.Errorf("second = %+v, want orphaned without a block", second)
	}

	none, err := payments.ListByInvoice(context.Background(), pool, "33333333-3333-3333-3333-333333333333")
	if err != nil || len(none) != 0 {
		t.Errorf("ListByInvoice of an unknown invoice = %v, %v; want nothing", none, err)
	}
}
