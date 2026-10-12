package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/money"
)

// Ошибки Post. Возвращаются как есть или обёрнутыми, проверять через errors.Is.
var (
	ErrEmptyKey          = errors.New("ledger: empty idempotency key")
	ErrEmptyKind         = errors.New("ledger: empty posting kind")
	ErrTooFewLines       = errors.New("ledger: posting needs at least two lines")
	ErrBadSide           = errors.New("ledger: line side must be debit or credit")
	ErrNonPositiveAmount = errors.New("ledger: line amount must be positive")
	ErrUnbalanced        = errors.New("ledger: lines do not sum to zero")
	ErrUnknownAccount    = errors.New("ledger: unknown account")
	ErrKeyConflict       = errors.New("ledger: idempotency key is already used with different content")
)

// KeyConflictError уточняет ErrKeyConflict: под ключом уже записана проводка вида StoredKind.
// Обработчику платежей это нужно, чтобы отличить штатный исход гонки (по платежу уже
// проведён другой исход: зачисление вместо возврата или наоборот) от ошибки в содержимом.
type KeyConflictError struct {
	Key        string
	StoredKind string
}

func (e *KeyConflictError) Error() string {
	return fmt.Sprintf("%v: key %q, stored kind %q", ErrKeyConflict, e.Key, e.StoredKind)
}

// Is сохраняет проверку errors.Is(err, ErrKeyConflict).
func (e *KeyConflictError) Is(target error) bool { return target == ErrKeyConflict }

// Side — сторона строки проводки.
type Side int8

const (
	SideDebit  Side = 1
	SideCredit Side = -1
)

// Line — одна строка проводки: счёт, сторона и сумма. Сумма всегда положительная,
// направление задаёт сторона. Нулевое значение Side недопустимо, поэтому строку,
// собранную без Debit или Credit, Post отклонит.
type Line struct {
	Account string
	Side    Side
	Amount  money.Amount
}

// Debit — дебет счёта на amount.
func Debit(account string, amount money.Amount) Line {
	return Line{Account: account, Side: SideDebit, Amount: amount}
}

// Credit — кредит счёта на amount.
func Credit(account string, amount money.Amount) Line {
	return Line{Account: account, Side: SideCredit, Amount: amount}
}

// signed возвращает сумму со знаком, как она хранится в журнале: дебет > 0, кредит < 0.
// Вызывать только после validate: сумма уже проверена на положительность.
func (l Line) signed() int64 {
	return int64(l.Side) * int64(l.Amount)
}

// Posting — одна операция по журналу: набор проводок под одним ключом идемпотентности.
// Ключ строится из платежа и шага: {payment_id}:{шаг}.
type Posting struct {
	Key   string // ключ идемпотентности
	Kind  string // вид операции: hold, credit, refund ...
	Lines []Line
}

// validate проверяет проводку, не обращаясь к базе.
func (p Posting) validate() error {
	if p.Key == "" {
		return ErrEmptyKey
	}
	if p.Kind == "" {
		return ErrEmptyKind
	}
	if len(p.Lines) < 2 {
		return fmt.Errorf("%w: got %d", ErrTooFewLines, len(p.Lines))
	}

	var sum int64
	for _, l := range p.Lines {
		if l.Side != SideDebit && l.Side != SideCredit {
			return fmt.Errorf("%w: account %q", ErrBadSide, l.Account)
		}
		// Ноль и минус отклоняем здесь, а не паникой в конструкторе: например, нулевую
		// комиссию вызывающий должен просто не добавлять строкой.
		if l.Amount <= 0 {
			return fmt.Errorf("%w: account %q, amount %d", ErrNonPositiveAmount, l.Account, int64(l.Amount))
		}
		a := l.signed()
		next := sum + a
		// Переполнение int64: иначе сумма могла бы «обернуться» ровно в ноль.
		if (a > 0 && next < sum) || (a < 0 && next > sum) {
			return fmt.Errorf("%w: sum overflows int64", ErrUnbalanced)
		}
		sum = next
	}
	if sum != 0 {
		return fmt.Errorf("%w: sum is %s", ErrUnbalanced, money.Amount(sum))
	}
	return nil
}

// Post записывает проводку в транзакцию вызывающего (статус платежа и проводка
// коммитятся вместе). Гарантии:
//   - posted = true: ключ был свободен, заголовок и все проводки записаны;
//   - posted = false, err = nil: проводка с этим ключом и тем же содержимым уже есть,
//     ничего не записано (безопасный повтор — после рестарта и при гонке тоже);
//   - ErrKeyConflict (*KeyConflictError с видом уже записанной проводки): ключ занят
//     проводкой с другим содержимым, ничего не записано;
//   - ErrUnknownAccount: счёта нет, ничего не записано.
//
// Параллельные вызовы с одним ключом выстраивает уникальный индекс: проигравший ждёт
// коммита победителя. Расчёт на READ COMMITTED (по умолчанию в postgres.InTx): после
// ожидания следующий запрос должен видеть закоммиченную проводку победителя.
//
// После любой ошибки вызывающий откатывает транзакцию.
func Post(ctx context.Context, tx pgx.Tx, p Posting) (posted bool, err error) {
	if err := p.validate(); err != nil {
		return false, err
	}

	// Счета ищем до записи: неизвестный счёт не оставляет в транзакции ни заголовка,
	// ни части проводок.
	accountIDs, err := lookupAccounts(ctx, tx, p.Lines)
	if err != nil {
		return false, err
	}

	var txID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger.transactions (idempotency_key, kind) VALUES ($1, $2)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, p.Key, p.Kind).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Ключ уже занят: ничего не вставляем, только сверяем содержимое.
		return false, checkStored(ctx, tx, p)
	}
	if err != nil {
		return false, fmt.Errorf("ledger: insert transaction %q: %w", p.Key, err)
	}

	for i, l := range p.Lines {
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger.entries (transaction_id, account_id, amount) VALUES ($1, $2, $3)`,
			txID, accountIDs[i], l.signed())
		if err != nil {
			return false, fmt.Errorf("ledger: insert entry for %q: %w", l.Account, err)
		}
	}
	return true, nil
}

// lookupAccounts возвращает id счетов в порядке строк; отсутствующий счёт — ErrUnknownAccount.
func lookupAccounts(ctx context.Context, tx pgx.Tx, lines []Line) ([]int64, error) {
	ids := make([]int64, len(lines))
	for i, l := range lines {
		err := tx.QueryRow(ctx, `SELECT id FROM ledger.accounts WHERE code = $1`, l.Account).Scan(&ids[i])
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownAccount, l.Account)
		}
		if err != nil {
			return nil, fmt.Errorf("ledger: look up account %q: %w", l.Account, err)
		}
	}
	return ids, nil
}

// checkStored сверяет сохранённую под ключом проводку с запрошенной: тот же вид и те же
// строки (порядок не важен) — nil, иначе ErrKeyConflict.
func checkStored(ctx context.Context, tx pgx.Tx, p Posting) error {
	var txID int64
	var kind string
	err := tx.QueryRow(ctx, `SELECT id, kind FROM ledger.transactions WHERE idempotency_key = $1`,
		p.Key).Scan(&txID, &kind)
	if err != nil {
		// Вставка только что упёрлась в этот заголовок, он обязан быть виден.
		return fmt.Errorf("ledger: load transaction %q: %w", p.Key, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT a.code, e.amount
		FROM ledger.entries e JOIN ledger.accounts a ON a.id = e.account_id
		WHERE e.transaction_id = $1`, txID)
	if err != nil {
		return fmt.Errorf("ledger: load entries of %q: %w", p.Key, err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil {
		return fmt.Errorf("ledger: load entries of %q: %w", p.Key, err)
	}

	if kind != p.Kind || !sameEntries(stored, entriesOf(p.Lines)) {
		return &KeyConflictError{Key: p.Key, StoredKind: kind}
	}
	return nil
}

// entry — строка проводки в том виде, в каком она лежит в журнале: сумма со знаком.
type entry struct {
	Account string
	Amount  int64
}

func entriesOf(lines []Line) []entry {
	out := make([]entry, len(lines))
	for i, l := range lines {
		out[i] = entry{Account: l.Account, Amount: l.signed()}
	}
	return out
}

// sameEntries сравнивает наборы строк без учёта порядка.
func sameEntries(a, b []entry) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, compareEntries)
	slices.SortFunc(b, compareEntries)
	return slices.Equal(a, b)
}

func compareEntries(x, y entry) int {
	return cmp.Or(strings.Compare(x.Account, y.Account), cmp.Compare(x.Amount, y.Amount))
}
