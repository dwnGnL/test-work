package payments_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/payments"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/platform/postgres/pgtest"
)

// Ветка 1: платежа нет — вставляется detected со всеми полями.
func TestDetectNewPayment(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 5))

	list, err := payments.ListByInvoice(context.Background(), pool, invoiceA)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByInvoice = %v, %v; want one payment", list, err)
	}
	p := list[0]
	if p.Status != payments.StatusDetected || p.InvoiceID != invoiceA || p.PartnerID != 7 || p.TxHash != "0xaa" ||
		p.LogIndex != 0 || p.From != sender || p.Amount != amt(t, "100") || p.FeeBps != fee {
		t.Errorf("payment = %+v", p)
	}
	if p.BlockNumber == nil || *p.BlockNumber != 5 || p.BlockHash == nil || *p.BlockHash != blockHash(5) {
		t.Errorf("block = %v %v, want 5 %s", p.BlockNumber, p.BlockHash, blockHash(5))
	}
}

// Ветка 2: платёж в orphaned возвращается в detected с новым блоком; запись та же.
func TestDetectOrphanedReturnsToDetected(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 5))
	before := find(t, pool, "0xaa", 0)
	setStatus(t, pool, before.ID, payments.StatusOrphaned)

	again := params(t, "aa", 0, 8)
	again.BlockHash = "0xnewblock"
	mustDetect(t, pool, again)

	after := find(t, pool, "0xaa", 0)
	if after.ID != before.ID || after.Status != payments.StatusDetected {
		t.Errorf("after = %+v, want the same payment %d in detected", after, before.ID)
	}
	if after.BlockNumber == nil || *after.BlockNumber != 8 || after.BlockHash == nil || *after.BlockHash != "0xnewblock" {
		t.Errorf("block = %v %v, want 8 0xnewblock", after.BlockNumber, after.BlockHash)
	}
	if n := countPayments(t, pool); n != 1 {
		t.Errorf("payments = %d, want 1", n)
	}
}

// Ветка 3: платёж в detected получает новые номер и хэш блока.
func TestDetectDetectedUpdatesBlock(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	mustDetect(t, pool, params(t, "aa", 0, 5))
	before := find(t, pool, "0xaa", 0)

	moved := params(t, "aa", 0, 9)
	moved.BlockHash = "0xmoved"
	mustDetect(t, pool, moved)

	after := find(t, pool, "0xaa", 0)
	if after.ID != before.ID || after.Status != payments.StatusDetected ||
		*after.BlockNumber != 9 || *after.BlockHash != "0xmoved" {
		t.Errorf("after = %+v (block %d %s), want the same payment in block 9", after, *after.BlockNumber, *after.BlockHash)
	}
}

// Ветка 4: aml_pending, credited и rejected не меняются.
func TestDetectKeepsConfirmedPayments(t *testing.T) {
	for _, status := range []payments.Status{payments.StatusAMLPending, payments.StatusCredited, payments.StatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			pool := pgtest.NewMigrated(t)
			mustDetect(t, pool, params(t, "aa", 0, 5))
			before := find(t, pool, "0xaa", 0)
			setStatus(t, pool, before.ID, status)

			moved := params(t, "aa", 0, 9)
			moved.BlockHash = "0xmoved"
			mustDetect(t, pool, moved)

			after := find(t, pool, "0xaa", 0)
			if after.ID != before.ID || after.Status != status ||
				*after.BlockNumber != 5 || *after.BlockHash != blockHash(5) {
				t.Errorf("after = %+v (block %d %s), want %s in block 5 unchanged", after, *after.BlockNumber, *after.BlockHash, status)
			}
		})
	}
}

// Повторный Detect того же перевода не создаёт второй записи; другой log_index или другая
// транзакция — другой платёж. Регистр хэша значения не имеет.
func TestDetectIsIdempotentByKey(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	for range 3 {
		mustDetect(t, pool, params(t, "aa", 0, 5))
	}
	if n := countPayments(t, pool); n != 1 {
		t.Fatalf("payments after repeated Detect = %d, want 1", n)
	}

	upper := params(t, "aa", 0, 5)
	upper.TxHash = "0xAA"
	upper.From = "0x" + strings.ToUpper(sender[2:])
	mustDetect(t, pool, upper)
	if n := countPayments(t, pool); n != 1 {
		t.Errorf("payments after Detect with an upper-case hash = %d, want 1", n)
	}
	var from string
	if err := pool.QueryRow(context.Background(), `SELECT from_address FROM payments.payments`).Scan(&from); err != nil || from != sender {
		t.Errorf("from_address = %q, %v; want %q", from, err, sender)
	}

	mustDetect(t, pool, params(t, "aa", 1, 5)) // та же транзакция, второе событие
	mustDetect(t, pool, params(t, "bb", 0, 5)) // другая транзакция
	if n := countPayments(t, pool); n != 3 {
		t.Errorf("payments = %d, want 3", n)
	}
}

// Параллельный Detect одного перевода: одна запись.
func TestDetectConcurrent(t *testing.T) {
	pool := pgtest.NewMigrated(t)
	pgtest.WarmUp(t, pool)

	const workers = 20
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- detect(context.Background(), pool, params(t, "aa", 0, 5))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Detect: %v", err)
		}
	}
	if n := countPayments(t, pool); n != 1 {
		t.Errorf("payments = %d, want 1", n)
	}
}

func TestDetectRejectsInvalid(t *testing.T) {
	pool := pgtest.NewMigrated(t)

	tests := []struct {
		name   string
		mutate func(*payments.DetectParams)
		want   error
	}{
		{"zero amount", func(p *payments.DetectParams) { p.Amount = 0 }, payments.ErrNonPositiveAmount},
		{"negative amount", func(p *payments.DetectParams) { p.Amount = -1 }, payments.ErrNonPositiveAmount},
		{"fee over 100%", func(p *payments.DetectParams) { p.FeeBps = 10001 }, payments.ErrInvalidFeeBps},
		{"negative fee", func(p *payments.DetectParams) { p.FeeBps = -1 }, payments.ErrInvalidFeeBps},
		{"empty invoice", func(p *payments.DetectParams) { p.InvoiceID = "" }, payments.ErrInvalidParams},
		{"no partner", func(p *payments.DetectParams) { p.PartnerID = 0 }, payments.ErrInvalidParams},
		{"empty tx hash", func(p *payments.DetectParams) { p.TxHash = "" }, payments.ErrInvalidParams},
		{"negative log index", func(p *payments.DetectParams) { p.LogIndex = -1 }, payments.ErrInvalidParams},
		{"empty sender", func(p *payments.DetectParams) { p.From = "" }, payments.ErrInvalidParams},
		{"negative block", func(p *payments.DetectParams) { p.BlockNumber = -1 }, payments.ErrInvalidParams},
		{"empty block hash", func(p *payments.DetectParams) { p.BlockHash = "" }, payments.ErrInvalidParams},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params(t, "aa", 0, 5)
			tt.mutate(&p)
			if err := detect(context.Background(), pool, p); !errors.Is(err, tt.want) {
				t.Errorf("Detect error = %v, want %v", err, tt.want)
			}
		})
	}
	if n := countPayments(t, pool); n != 0 {
		t.Errorf("payments = %d, want 0: rejected calls must not write", n)
	}
}

// Detect пишет в транзакцию вызывающего: откат убирает и платёж.
func TestDetectRollsBackWithCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	boom := errors.New("boom")
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := payments.Detect(ctx, tx, params(t, "aa", 0, 5)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx error = %v, want boom", err)
	}
	if n := countPayments(t, pool); n != 0 {
		t.Errorf("payments = %d after rollback, want 0", n)
	}
}

// OrphanFrom трогает только detected из блоков >= fromBlock и считает прошедшие порог.
func TestOrphanFrom(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewMigrated(t)

	// detected: блоки 5, 6, 7; aml_pending: 6; credited: 7; rejected: 8.
	mustDetect(t, pool, params(t, "d5", 0, 5))
	mustDetect(t, pool, params(t, "d6", 0, 6))
	mustDetect(t, pool, params(t, "d7", 0, 7))
	for tx, c := range map[string]struct {
		block  int64
		status payments.Status
	}{
		"p6": {6, payments.StatusAMLPending}, "c7": {7, payments.StatusCredited}, "r8": {8, payments.StatusRejected},
	} {
		mustDetect(t, pool, params(t, tx, 0, c.block))
		setStatus(t, pool, find(t, pool, "0x"+tx, 0).ID, c.status)
	}

	var orphaned, confirmed int
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		orphaned, confirmed, err = payments.OrphanFrom(ctx, tx, 6)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if orphaned != 2 || confirmed != 3 {
		t.Errorf("OrphanFrom(6) = %d, %d; want 2 orphaned and 3 past the threshold", orphaned, confirmed)
	}

	for _, tt := range []struct {
		tx     string
		status payments.Status
		block  int64 // 0 — блока нет
	}{
		{"d5", payments.StatusDetected, 5}, // ниже fromBlock
		{"d6", payments.StatusOrphaned, 0},
		{"d7", payments.StatusOrphaned, 0},
		{"p6", payments.StatusAMLPending, 6}, // не тронуты
		{"c7", payments.StatusCredited, 7},
		{"r8", payments.StatusRejected, 8},
	} {
		got := find(t, pool, "0x"+tt.tx, 0)
		if got.Status != tt.status {
			t.Errorf("%s: status = %s, want %s", tt.tx, got.Status, tt.status)
		}
		if tt.block == 0 && (got.BlockNumber != nil || got.BlockHash != nil) {
			t.Errorf("%s: block %v %v, want none", tt.tx, got.BlockNumber, got.BlockHash)
		}
		if tt.block != 0 && (got.BlockNumber == nil || *got.BlockNumber != tt.block) {
			t.Errorf("%s: block %v, want %d", tt.tx, got.BlockNumber, tt.block)
		}
	}

	// Повтор ничего не находит; откат транзакции возвращает всё как было.
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		orphaned, confirmed, err = payments.OrphanFrom(ctx, tx, 6)
		return err
	})
	if err != nil || orphaned != 0 || confirmed != 3 {
		t.Errorf("repeated OrphanFrom(6) = %d, %d, %v; want 0 orphaned and 3 past the threshold", orphaned, confirmed, err)
	}
}
