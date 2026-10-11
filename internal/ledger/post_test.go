package ledger_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dwnGnL/test-work/internal/ledger"
	"github.com/dwnGnL/test-work/internal/money"
)

// holdPosting — шаг 1 из дизайна: платёж 100 USDT прошёл порог и ждёт решения AML.
func holdPosting(t *testing.T, key string) ledger.Posting {
	return ledger.Posting{Key: key, Kind: "hold", Lines: []ledger.Line{
		ledger.Debit(ledger.AccountDeposits, amt(t, "100")),
		ledger.Credit(ledger.AccountHoldAML, amt(t, "100")),
	}}
}

// creditPosting — шаг 2a: AML пропустил, 98.5 партнёру и 1.5 комиссии.
func creditPosting(t *testing.T, key string, partnerID int64) ledger.Posting {
	return ledger.Posting{Key: key, Kind: "credit", Lines: []ledger.Line{
		ledger.Debit(ledger.AccountHoldAML, amt(t, "100")),
		ledger.Credit(ledger.PartnerAccount(partnerID), amt(t, "98.5")),
		ledger.Credit(ledger.AccountRevenueFees, amt(t, "1.5")),
	}}
}

// refundPosting — шаг 2b: AML отклонил, 100 к возврату отправителю.
func refundPosting(t *testing.T, key string) ledger.Posting {
	return ledger.Posting{Key: key, Kind: "refund", Lines: []ledger.Line{
		ledger.Debit(ledger.AccountHoldAML, amt(t, "100")),
		ledger.Credit(ledger.AccountRefundsPayable, amt(t, "100")),
	}}
}

// Пример из дизайна: 100 USDT в холд, затем 98.5 партнёру и 1.5 комиссии.
// Балансы счетов хранятся раздельно, а сумма сальдо по всем счетам всегда ноль.
func TestPostDoubleEntry(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	partner := ledger.PartnerAccount(7)
	if err := ensure(ctx, pool, partner); err != nil {
		t.Fatal(err)
	}

	mustPost(t, pool, holdPosting(t, "p1:1"))
	assertTotals(t, pool, map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "100"),
		ledger.AccountHoldAML:        -amt(t, "100"),
		ledger.AccountRefundsPayable: 0,
		ledger.AccountRevenueFees:    0,
		partner:                      0,
	})

	mustPost(t, pool, creditPosting(t, "p1:2", 7))
	assertTotals(t, pool, map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "100"),
		ledger.AccountHoldAML:        0,
		ledger.AccountRefundsPayable: 0,
		ledger.AccountRevenueFees:    -amt(t, "1.5"),
		partner:                      -amt(t, "98.5"),
	})

	// Второй платёж уходит в возврат: холд пуст, долг перед отправителем на refunds:payable.
	mustPost(t, pool, holdPosting(t, "p2:1"))
	mustPost(t, pool, refundPosting(t, "p2:2"))
	assertTotals(t, pool, map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "200"),
		ledger.AccountHoldAML:        0,
		ledger.AccountRefundsPayable: -amt(t, "100"),
		ledger.AccountRevenueFees:    -amt(t, "1.5"),
		partner:                      -amt(t, "98.5"),
	})
}

// Повтор с тем же ключом и теми же строками ничего не пишет: ни при повторной обработке,
// ни после рестарта (новая транзакция, новое соединение), ни при другом порядке строк.
func TestPostRepeatIsNoop(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	if err := ensure(ctx, pool, ledger.PartnerAccount(7)); err != nil {
		t.Fatal(err)
	}
	p := creditPosting(t, "p1:2", 7)
	mustPost(t, pool, p)
	txs, entries := journalSize(t, pool)
	if txs != 1 || entries != 3 {
		t.Fatalf("after first Post: %d transactions, %d entries; want 1 and 3", txs, entries)
	}

	reordered := p
	reordered.Lines = []ledger.Line{p.Lines[2], p.Lines[0], p.Lines[1]}
	for name, again := range map[string]ledger.Posting{"same lines": p, "other order": reordered} {
		t.Run(name, func(t *testing.T) {
			posted, err := post(ctx, pool, again)
			if posted || err != nil {
				t.Errorf("repeat: posted = %v, err = %v; want false, nil", posted, err)
			}
			if txs2, entries2 := journalSize(t, pool); txs2 != txs || entries2 != entries {
				t.Errorf("journal changed: %d transactions, %d entries; want %d and %d", txs2, entries2, txs, entries)
			}
		})
	}
	assertTotals(t, pool, map[string]money.Amount{
		ledger.AccountDeposits:       0,
		ledger.AccountHoldAML:        amt(t, "100"),
		ledger.AccountRefundsPayable: 0,
		ledger.AccountRevenueFees:    -amt(t, "1.5"),
		ledger.PartnerAccount(7):     -amt(t, "98.5"),
	})
}

// Тот же ключ, другое содержимое: например, по одному платежу пытаются провести и
// зачисление, и возврат. Вторая проводка не проходит, журнал не меняется.
func TestPostKeyConflict(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	if err := ensure(ctx, pool, ledger.PartnerAccount(7)); err != nil {
		t.Fatal(err)
	}
	original := creditPosting(t, "p1:2", 7)
	mustPost(t, pool, original)
	txs, entries := journalSize(t, pool)

	tests := []struct {
		name string
		p    ledger.Posting
	}{
		{"refund instead of credit", refundPosting(t, "p1:2")},
		{"different amount", ledger.Posting{Key: "p1:2", Kind: "credit", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountHoldAML, amt(t, "100")),
			ledger.Credit(ledger.PartnerAccount(7), amt(t, "99")),
			ledger.Credit(ledger.AccountRevenueFees, amt(t, "1")),
		}}},
		{"different account", ledger.Posting{Key: "p1:2", Kind: "credit", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountHoldAML, amt(t, "100")),
			ledger.Credit(ledger.PartnerAccount(7), amt(t, "98.5")),
			ledger.Credit(ledger.AccountRefundsPayable, amt(t, "1.5")),
		}}},
		{"fewer lines", ledger.Posting{Key: "p1:2", Kind: "credit", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountHoldAML, amt(t, "98.5")),
			ledger.Credit(ledger.PartnerAccount(7), amt(t, "98.5")),
		}}},
		{"different kind", ledger.Posting{Key: "p1:2", Kind: "other", Lines: original.Lines}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted, err := post(ctx, pool, tt.p)
			if posted || !errors.Is(err, ledger.ErrKeyConflict) {
				t.Errorf("posted = %v, err = %v; want false, ErrKeyConflict", posted, err)
			}
			if txs2, entries2 := journalSize(t, pool); txs2 != txs || entries2 != entries {
				t.Errorf("journal changed: %d transactions, %d entries; want %d and %d", txs2, entries2, txs, entries)
			}
		})
	}

	// Исходная проводка по-прежнему узнаётся как повтор.
	if posted, err := post(ctx, pool, original); posted || err != nil {
		t.Errorf("original repeat: posted = %v, err = %v; want false, nil", posted, err)
	}
}

// Некорректная проводка отклоняется до обращения к базе: вместо транзакции передаём nil,
// при любом обращении к базе тест упал бы с паникой. Базы для этого теста не нужно.
func TestPostRejectsInvalidBeforeDB(t *testing.T) {
	a100 := money.Amount(100_000_000)
	valid := func() ledger.Posting {
		return ledger.Posting{Key: "k:1", Kind: "test", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountDeposits, a100),
			ledger.Credit(ledger.AccountHoldAML, a100),
		}}
	}
	with := func(change func(*ledger.Posting)) ledger.Posting {
		p := valid()
		change(&p)
		return p
	}

	tests := []struct {
		name string
		p    ledger.Posting
		want error
	}{
		{"empty key", with(func(p *ledger.Posting) { p.Key = "" }), ledger.ErrEmptyKey},
		{"empty kind", with(func(p *ledger.Posting) { p.Kind = "" }), ledger.ErrEmptyKind},
		{"no lines", with(func(p *ledger.Posting) { p.Lines = nil }), ledger.ErrTooFewLines},
		{"one line", with(func(p *ledger.Posting) { p.Lines = p.Lines[:1] }), ledger.ErrTooFewLines},
		{"zero amount", with(func(p *ledger.Posting) {
			p.Lines = append(p.Lines, ledger.Credit(ledger.AccountRevenueFees, 0))
		}), ledger.ErrZeroAmount},
		{"debit exceeds credit", with(func(p *ledger.Posting) {
			p.Lines[1] = ledger.Credit(ledger.AccountHoldAML, a100-1)
		}), ledger.ErrUnbalanced},
		{"credit exceeds debit", with(func(p *ledger.Posting) {
			p.Lines[0] = ledger.Debit(ledger.AccountDeposits, a100-1)
		}), ledger.ErrUnbalanced},
		{"debits only", with(func(p *ledger.Posting) {
			p.Lines[1] = ledger.Debit(ledger.AccountHoldAML, a100)
		}), ledger.ErrUnbalanced},
		// 2^63-1 + 2^63-1 + 2 = 2^64: в int64 такая сумма «оборачивается» ровно в ноль.
		{"sum wraps around to zero", ledger.Posting{Key: "k:2", Kind: "test", Lines: []ledger.Line{
			ledger.Debit(ledger.AccountDeposits, math.MaxInt64),
			ledger.Debit(ledger.AccountHoldAML, math.MaxInt64),
			ledger.Debit(ledger.AccountRevenueFees, 2),
		}}, ledger.ErrUnbalanced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted, err := ledger.Post(context.Background(), nil, tt.p)
			if posted || !errors.Is(err, tt.want) {
				t.Errorf("posted = %v, err = %v; want false, %v", posted, err, tt.want)
			}
		})
	}
}

// Неизвестный счёт — явная ошибка, а не молча пропущенная строка. Проводка не оставляет
// в транзакции вызывающего ни заголовка, ни части строк (даже если неизвестен второй счёт),
// и транзакция остаётся рабочей: её можно откатить.
func TestPostUnknownAccount(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // на случай падения теста раньше явного отката

	p := ledger.Posting{Key: "p1:2", Kind: "credit", Lines: []ledger.Line{
		ledger.Debit(ledger.AccountDeposits, amt(t, "100")),
		ledger.Credit(ledger.PartnerAccount(404), amt(t, "100")), // счёт партнёра не создан
	}}
	posted, err := ledger.Post(ctx, tx, p)
	if posted || !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("posted = %v, err = %v; want false, ErrUnknownAccount", posted, err)
	}

	// Запрос внутри транзакции работает (она не прервана), и в ней ничего не записано.
	if txs, entries := journalSize(t, tx); txs != 0 || entries != 0 {
		t.Errorf("partial write: %d transactions, %d entries; want 0 and 0", txs, entries)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("rollback: %v", err)
	}
	if txs, entries := journalSize(t, pool); txs != 0 || entries != 0 {
		t.Errorf("after rollback: %d transactions, %d entries; want 0 and 0", txs, entries)
	}

	// Ключ не занят: после создания счёта та же проводка проходит.
	if err := ensure(ctx, pool, ledger.PartnerAccount(404)); err != nil {
		t.Fatal(err)
	}
	mustPost(t, pool, p)
}

// 20 горутин проводят одну проводку, каждая в своей транзакции базы: ровно одна записывает,
// остальные ждут коммита победителя и видят повтор. Проводок ровно две.
func TestPostConcurrentSameKey(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	p := holdPosting(t, "p1:1")

	warmUp(t, pool)
	const workers = 20
	type result struct {
		posted bool
		err    error
	}
	results := make([]result, workers)
	start := make(chan struct{}) // все стартуют одновременно
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i].posted, results[i].err = post(ctx, pool, p)
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("worker %d: %v", i, r.err)
		}
		if r.posted {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d workers got posted = true, want exactly 1", winners)
	}
	if txs, entries := journalSize(t, pool); txs != 1 || entries != 2 {
		t.Errorf("journal: %d transactions, %d entries; want 1 and 2", txs, entries)
	}
	assertTotals(t, pool, map[string]money.Amount{
		ledger.AccountDeposits:       amt(t, "100"),
		ledger.AccountHoldAML:        -amt(t, "100"),
		ledger.AccountRefundsPayable: 0,
		ledger.AccountRevenueFees:    0,
	})
}

// Debit и Credit задают знак: дебет > 0, кредит < 0. Базы не нужно.
func TestLineSigns(t *testing.T) {
	if got := ledger.Debit("a", 5); got != (ledger.Line{Account: "a", Amount: 5}) {
		t.Errorf("Debit = %+v, want amount +5", got)
	}
	if got := ledger.Credit("a", 5); got != (ledger.Line{Account: "a", Amount: -5}) {
		t.Errorf("Credit = %+v, want amount -5", got)
	}
	if got := ledger.PartnerAccount(42); got != "partner:42" {
		t.Errorf("PartnerAccount(42) = %q, want partner:42", got)
	}

	// Отрицательная сумма — ошибка программиста: молча перевернуть знак нельзя.
	for name, build := range map[string]func(string, money.Amount) ledger.Line{"Debit": ledger.Debit, "Credit": ledger.Credit} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s with a negative amount did not panic", name)
				}
			}()
			build("a", -1)
		}()
	}
}

// Транзакция вызывающего откатывается целиком: если её откатили после успешного Post,
// проводка исчезает вместе с заголовком, а ключ снова свободен.
func TestPostRollsBackWithCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	p := holdPosting(t, "p1:1")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // иначе упавший тест повесит закрытие пула
	if posted, err := ledger.Post(ctx, tx, p); !posted || err != nil {
		t.Fatalf("Post = %v, %v; want true, nil", posted, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if txs, entries := journalSize(t, pool); txs != 0 || entries != 0 {
		t.Errorf("after rollback: %d transactions, %d entries; want 0 and 0", txs, entries)
	}
	mustPost(t, pool, p) // ключ не занят откатившейся транзакцией
}
