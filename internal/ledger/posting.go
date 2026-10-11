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
	ErrEmptyKey       = errors.New("ledger: empty idempotency key")
	ErrEmptyKind      = errors.New("ledger: empty posting kind")
	ErrTooFewLines    = errors.New("ledger: posting needs at least two lines")
	ErrZeroAmount     = errors.New("ledger: line with zero amount")
	ErrUnbalanced     = errors.New("ledger: lines do not sum to zero")
	ErrUnknownAccount = errors.New("ledger: unknown account")
	ErrKeyConflict    = errors.New("ledger: idempotency key is already used with different content")
)

// Line — одна проводка со знаком: дебет > 0, кредит < 0.
type Line struct {
	Account string
	Amount  money.Amount
}

// Debit — дебет счёта на amount (amount > 0). Ноль не паникует: Post отклонит его
// как ErrZeroAmount (например, нулевую комиссию вызывающий должен просто не добавлять).
// Отрицательная сумма — ошибка программиста: знак задаёт конструктор, а не вызывающий,
// иначе дебет молча превратился бы в кредит. Как и в money.FeeBps, это паника.
func Debit(account string, amount money.Amount) Line {
	mustNotBeNegative(amount)
	return Line{Account: account, Amount: amount}
}

// Credit — кредит счёта на amount (amount > 0), в проводке хранится как -amount.
// Правила для нуля и отрицательных сумм те же, что у Debit.
func Credit(account string, amount money.Amount) Line {
	mustNotBeNegative(amount)
	return Line{Account: account, Amount: -amount}
}

func mustNotBeNegative(amount money.Amount) {
	if amount < 0 {
		panic(fmt.Sprintf("ledger: Debit/Credit: negative amount %d", int64(amount)))
	}
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
		a := int64(l.Amount)
		if a == 0 {
			return fmt.Errorf("%w: account %q", ErrZeroAmount, l.Account)
		}
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
//   - ErrKeyConflict: ключ занят проводкой с другим содержимым, ничего не записано;
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
			txID, accountIDs[i], int64(l.Amount))
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
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Line])
	if err != nil {
		return fmt.Errorf("ledger: load entries of %q: %w", p.Key, err)
	}

	if kind != p.Kind || !sameLines(stored, p.Lines) {
		return fmt.Errorf("%w: %q", ErrKeyConflict, p.Key)
	}
	return nil
}

// sameLines сравнивает наборы строк без учёта порядка.
func sameLines(a, b []Line) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, compareLines)
	slices.SortFunc(b, compareLines)
	return slices.Equal(a, b)
}

func compareLines(x, y Line) int {
	return cmp.Or(strings.Compare(x.Account, y.Account), cmp.Compare(x.Amount, y.Amount))
}
