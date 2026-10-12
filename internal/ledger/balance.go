package ledger

import (
	"context"
	"fmt"

	"github.com/dwnGnL/test-work/internal/money"
)

// Balance возвращает сальдо счёта: сумму его проводок со знаком (дебет > 0, кредит < 0).
// Счёт без проводок даёт 0, несуществующий счёт — ErrUnknownAccount.
func Balance(ctx context.Context, q Querier, code string) (money.Amount, error) {
	sum, found, err := selectBalance(ctx, q, code)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("%w: %q", ErrUnknownAccount, code)
	}
	return sum, nil
}

// PartnerBalance возвращает баланс партнёра как положительное число.
//
// Счёт партнёра — обязательство сервиса перед ним: зачисление идёт кредитом (проводка < 0),
// поэтому сальдо счёта отрицательное. Партнёру показываем то же число с обратным знаком:
// положительное, если сервис ему должен.
func PartnerBalance(ctx context.Context, q Querier, partnerID int64) (money.Amount, error) {
	sum, err := Balance(ctx, q, PartnerAccount(partnerID))
	if err != nil {
		return 0, err
	}
	return -sum, nil
}

// Totals возвращает сальдо по всем счетам (со знаком, как Balance), включая счета без проводок.
// Сумма по всем счетам обязана быть нулём; саму проверку делает CheckBalanced. Читает всю
// таблицу проводок, в обработчиках запросов её вызывать не нужно.
func Totals(ctx context.Context, q Querier) (map[string]money.Amount, error) {
	return selectTotals(ctx, q)
}

// CheckBalanced проверяет главный инвариант двойной записи: сумма всех проводок равна нулю.
// Триггер базы гарантирует это для каждой транзакции журнала, поэтому ошибка здесь означает,
// что журнал меняли в обход триггеров. Нужна сверке и тестам; читает всю таблицу проводок.
func CheckBalanced(ctx context.Context, q Querier) error {
	sum, err := selectEntriesSum(ctx, q)
	if err != nil {
		return err
	}
	if sum != 0 {
		return fmt.Errorf("%w: sum of all entries is %s", ErrBooksUnbalanced, sum)
	}
	return nil
}
