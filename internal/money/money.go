// Package money хранит суммы USDT целыми числами в минимальных единицах.
//
// Одна единица Amount — 10⁻⁶ USDT (точность токена — 6 знаков). Типов с плавающей
// точкой здесь нет и быть не должно: разбор, вывод и расчёт комиссии целочисленные.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Decimals — число знаков после точки у USDT.
const Decimals = 6

// Amount — сумма в минимальных единицах (10⁻⁶ USDT).
type Amount int64

// Ошибки Parse. Возвращаются обёрнутыми, проверять через errors.Is.
var (
	ErrSyntax    = errors.New("money: invalid syntax")
	ErrPrecision = errors.New("money: too many decimal places")
	ErrOverflow  = errors.New("money: amount overflows int64")
)

const scale = 1_000_000 // 10^Decimals

// Parse строго разбирает десятичную запись суммы: цифры, опционально одна точка
// и цифры после неё (не больше Decimals). Целая и дробная части обязательны:
// ".5" и "5." — ошибка. Знак, пробелы, экспонента, разделители тысяч запрещены.
// Лишние ведущие нули допустимы ("007.5"). Нулевая сумма — не ошибка разбора,
// проверка «больше нуля» остаётся за вызывающим.
func Parse(s string) (Amount, error) {
	whole, frac, hasDot := strings.Cut(s, ".")
	if !isDigits(whole) || (hasDot && !isDigits(frac)) {
		return 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	if len(frac) > Decimals {
		return 0, fmt.Errorf("%w: %q", ErrPrecision, s)
	}

	// Собираем целое число из цифр целой части и дробной, дополненной нулями
	// до Decimals знаков; переполнение проверяем до умножения.
	var v int64
	for i := 0; i < len(whole)+Decimals; i++ {
		var d int64
		switch {
		case i < len(whole):
			d = int64(whole[i] - '0')
		case i-len(whole) < len(frac):
			d = int64(frac[i-len(whole)] - '0')
		}
		if v > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
		}
		v = v*10 + d
	}
	return Amount(v), nil
}

// isDigits истинно для непустой строки из ASCII-цифр (unicode.IsDigit не годится:
// он пропускает цифры других алфавитов).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String возвращает сумму всегда с Decimals знаками: "100.000000", "0.000001".
// Отрицательные значения (дебеты и кредиты в отчётах) выводятся со знаком.
func (a Amount) String() string {
	// Модуль считаем в uint64: так корректно обрабатывается и math.MinInt64.
	u := uint64(a)
	if a < 0 {
		u = -u
	}
	buf := make([]byte, 0, 28)
	if a < 0 {
		buf = append(buf, '-')
	}
	buf = strconv.AppendUint(buf, u/scale, 10)
	buf = append(buf, '.')
	frac := strconv.FormatUint(u%scale, 10)
	buf = append(buf, strings.Repeat("0", Decimals-len(frac))...)
	buf = append(buf, frac...)
	return string(buf)
}

// MarshalJSON кодирует сумму JSON-строкой: число в JSON читают как float.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON принимает только JSON-строку в формате Parse. Число и null
// отклоняются: пропущенная сумма не должна молча стать нулём.
func (a *Amount) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("money: amount must not be null")
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("money: amount must be a JSON string: %w", err)
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// maxBps — 100% в базисных пунктах.
const maxBps = 10_000

// FeeBps возвращает комиссию в базисных пунктах: floor((amount × bps + 5000) / 10000),
// то есть округление до ближайшего, половина вверх.
//
// Произведение amount × bps может не поместиться в int64, поэтому его не считаем.
// Пусть amount = 10000·q + r, 0 ≤ r < 10000. Слагаемое 10000·q·bps делится на 10000
// нацело, его можно вынести из-под floor: результат = q·bps + floor((r·bps + 5000) / 10000).
// Переполнения нет: q·bps ≤ amount (bps ≤ 10000), а r·bps + 5000 < 10⁸.
//
// Аргументы вне допустимых границ (amount < 0, bps вне 0..10000) — ошибка
// программиста, а не данных: в базе их не пропускают CHECK-ограничения.
// Функция паникует, чтобы такая ошибка не превратилась в неверную комиссию.
func FeeBps(amount Amount, bps int) Amount {
	if amount < 0 || bps < 0 || bps > maxBps {
		panic(fmt.Sprintf("money: FeeBps: invalid arguments amount=%d bps=%d", int64(amount), bps))
	}
	a, b := int64(amount), int64(bps)
	q, r := a/maxBps, a%maxBps
	return Amount(q*b + (r*b+maxBps/2)/maxBps)
}
