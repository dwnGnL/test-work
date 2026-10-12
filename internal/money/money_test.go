package money

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Amount
	}{
		{"0", 0},
		{"0.0", 0},
		{"0.000000", 0},
		{"1", 1_000_000},
		{"100", 100_000_000},
		{"100.000000", 100_000_000},
		{"100.5", 100_500_000},
		{"0.1", 100_000},
		{"0.000001", 1},
		{"1.000001", 1_000_001},
		{"123456.789012", 123_456_789_012},
		{"007.5", 7_500_000}, // ведущие нули допустимы
		{"000000000000000000000000000001", 1_000_000},
		{"9223372036854.775806", math.MaxInt64 - 1},
		{"9223372036854.775807", math.MaxInt64}, // граница int64
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		// синтаксис
		{"empty", "", ErrSyntax},
		{"only dot", ".", ErrSyntax},
		{"no integer part", ".5", ErrSyntax},
		{"no fraction part", "1.", ErrSyntax},
		{"minus", "-1", ErrSyntax},
		{"minus zero", "-0", ErrSyntax},
		{"plus", "+1", ErrSyntax},
		{"leading space", " 1", ErrSyntax},
		{"trailing space", "1 ", ErrSyntax},
		{"inner space", "1 .5", ErrSyntax},
		{"trailing newline", "1.5\n", ErrSyntax},
		{"exponent", "1e3", ErrSyntax},
		{"exponent upper", "1E3", ErrSyntax},
		{"exponent with sign", "1.5e-3", ErrSyntax},
		{"hex", "0x10", ErrSyntax},
		{"comma", "1,5", ErrSyntax},
		{"thousands separator", "1_000", ErrSyntax},
		{"two dots", "1.2.3", ErrSyntax},
		{"two dots and long tail", "1.2.3456789", ErrSyntax},
		{"double dot", "1..5", ErrSyntax},
		{"sign in fraction", "1.-5", ErrSyntax},
		{"letters", "abc", ErrSyntax},
		{"nan", "NaN", ErrSyntax},
		{"inf", "Inf", ErrSyntax},
		{"arabic digits", "١٢٣", ErrSyntax},
		{"fullwidth digits", "１２３", ErrSyntax},
		// точность
		{"7 decimals", "0.0000001", ErrPrecision},
		{"7 decimals nonzero", "1.1234567", ErrPrecision},
		{"7 decimals trailing zero", "1.5000000", ErrPrecision}, // лишний ноль тоже отвергаем
		{"many decimals", "1.00000000000000000000", ErrPrecision},
		// переполнение
		{"max plus one", "9223372036854.775808", ErrOverflow},
		{"integer part too big", "9223372036855", ErrOverflow},
		{"int64 max as units", "9223372036854775807", ErrOverflow},
		{"huge", "99999999999999999999999999", ErrOverflow},
		{"huge with fraction", "99999999999999999999999999.999999", ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Parse(%q) = %d, %v; want error %v", tt.in, got, err, tt.want)
			}
			if got != 0 {
				t.Errorf("Parse(%q) returned %d together with error", tt.in, got)
			}
		})
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		in   Amount
		want string
	}{
		{0, "0.000000"},
		{1, "0.000001"},
		{999_999, "0.999999"},
		{1_000_000, "1.000000"},
		{1_500_000, "1.500000"},
		{100_000_000, "100.000000"},
		{math.MaxInt64, "9223372036854.775807"},
		{-1, "-0.000001"},
		{-1_500_000, "-1.500000"},
		{math.MinInt64, "-9223372036854.775808"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Amount(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
		}
	}
}

// Parse и String взаимно обратны на неотрицательных суммах.
func TestParseStringRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	check := func(a Amount) {
		t.Helper()
		got, err := Parse(a.String())
		if err != nil || got != a {
			t.Fatalf("Parse(%q) = %d, %v; want %d", a.String(), got, err, a)
		}
	}
	for _, a := range []Amount{0, 1, 999_999, 1_000_000, math.MaxInt64} {
		check(a)
	}
	for range 10_000 {
		check(Amount(rnd.Int64()))                   // все порядки значений вблизи верхней границы
		check(Amount(rnd.Int64N(1_000_000_000_000))) // реалистичные суммы
	}
}

func TestJSON(t *testing.T) {
	type payload struct {
		Amount Amount `json:"amount"`
	}

	t.Run("marshal as string", func(t *testing.T) {
		b, err := json.Marshal(payload{Amount: 100_500_000})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"amount":"100.500000"}`; string(b) != want {
			t.Errorf("got %s, want %s", b, want)
		}
	})

	t.Run("marshal pointer", func(t *testing.T) {
		a := Amount(1)
		b, err := json.Marshal(&a)
		if err != nil {
			t.Fatal(err)
		}
		if want := `"0.000001"`; string(b) != want {
			t.Errorf("got %s, want %s", b, want)
		}
	})

	t.Run("unmarshal", func(t *testing.T) {
		tests := map[string]Amount{
			`{"amount":"100"}`:        100_000_000,
			`{"amount":"100.000000"}`: 100_000_000,
			`{"amount":"0.000001"}`:   1,
			`{}`:                      0, // отсутствие поля — забота проверки «больше нуля»
		}
		for in, want := range tests {
			var p payload
			if err := json.Unmarshal([]byte(in), &p); err != nil {
				t.Errorf("Unmarshal(%s) error: %v", in, err)
				continue
			}
			if p.Amount != want {
				t.Errorf("Unmarshal(%s) = %d, want %d", in, p.Amount, want)
			}
		}
	})

	t.Run("unmarshal rejects", func(t *testing.T) {
		bad := []string{
			`{"amount":100}`,   // число вместо строки
			`{"amount":100.5}`, // дробное число
			`{"amount":1e2}`,   // экспонента
			`{"amount":null}`,
			`{"amount":true}`,
			`{"amount":{}}`,
			`{"amount":[]}`,
			`{"amount":""}`,
			`{"amount":"-1"}`,
			`{"amount":"1e3"}`,
			`{"amount":" 1"}`,
			`{"amount":"0.0000001"}`,
			`{"amount":"9223372036854.775808"}`,
		}
		for _, in := range bad {
			var p payload
			if err := json.Unmarshal([]byte(in), &p); err == nil {
				t.Errorf("Unmarshal(%s) = %d, want error", in, p.Amount)
			}
		}
	})

	t.Run("null pointer stays nil", func(t *testing.T) {
		var p struct {
			Amount *Amount `json:"amount"`
		}
		if err := json.Unmarshal([]byte(`{"amount":null}`), &p); err != nil {
			t.Fatal(err)
		}
		if p.Amount != nil {
			t.Errorf("got %v, want nil", *p.Amount)
		}
	})
}

func TestFeeBps(t *testing.T) {
	tests := []struct {
		name   string
		amount Amount
		bps    int
		want   Amount
	}{
		{"zero amount", 0, 150, 0},
		{"zero rate", 100_000_000, 0, 0},
		{"1.5 percent of 100", 100_000_000, 150, 1_500_000},
		{"0.5 percent of 100", 100_000_000, 50, 500_000},
		{"full amount", 100_000_000, 10_000, 100_000_000},
		{"half rounds up", 1, 5000, 1},         // 0.5 -> 1
		{"below half rounds down", 1, 4999, 0}, // 0.4999 -> 0
		{"1.5 rounds up", 3, 5000, 2},          // 1.5 -> 2
		{"exact", 2, 5000, 1},                  // 1.0
		{"tiny", 1, 1, 0},                      // 0.0001 -> 0
		{"0.9999 rounds up", 9999, 1, 1},       //
		{"0.5 exactly rounds up", 5000, 1, 1},  // 5000 * 1 / 10000 = 0.5 -> 1
		{"0.4999 rounds down", 4999, 1, 0},     //
		{"15000.015 rounds down", 1_000_001, 150, 15_000},
		{"4999.995 rounds up", 333_333, 150, 5000},
		{"max amount, full rate", math.MaxInt64, 10_000, math.MaxInt64},
		{"max amount, zero rate", math.MaxInt64, 0, 0},
		{"max amount, 1 bp", math.MaxInt64, 1, 922_337_203_685_478},             // 922337203685477.5807 -> ...478
		{"max amount, 150 bp", math.MaxInt64, 150, 138_350_580_552_821_637},     // без переполнения int64
		{"max amount, 9999 bp", math.MaxInt64, 9999, 9_222_449_699_651_090_329}, //
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := FeeBps(tt.amount, tt.bps); err != nil || got != tt.want {
				t.Errorf("FeeBps(%d, %d) = %d, %v; want %d, nil", int64(tt.amount), tt.bps, int64(got), err, int64(tt.want))
			}
			// Ожидание из таблицы не должно расходиться с эталонным расчётом.
			if want := feeBig(tt.amount, tt.bps); tt.want != want {
				t.Errorf("table value %d disagrees with big.Int reference %d", int64(tt.want), int64(want))
			}
		})
	}
}

// feeBig — эталон на math/big: та же формула без риска переполнения.
func feeBig(amount Amount, bps int) Amount {
	x := new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(int64(bps)))
	x.Add(x, big.NewInt(5000))
	x.Quo(x, big.NewInt(10_000))
	return Amount(x.Int64())
}

func TestFeeBpsMatchesBigIntReference(t *testing.T) {
	rnd := rand.New(rand.NewPCG(42, 7))
	amounts := []Amount{0, 1, 2, 3, 4999, 5000, 5001, 9999, 10_000, 10_001, 1_000_000,
		math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 / 2, math.MaxInt64/2 + 1, 1 << 62, 1<<62 - 1,
		math.MaxInt64 - 5807, math.MaxInt64 - 5808} // остаток от деления на 10000: 0 и 9999 при максимальном частном
	rates := []int{0, 1, 2, 50, 150, 4999, 5000, 5001, 9999, 10_000}

	check := func(a Amount, bps int) {
		t.Helper()
		got, err := FeeBps(a, bps)
		want := feeBig(a, bps)
		if err != nil || got != want {
			t.Fatalf("FeeBps(%d, %d) = %d, %v; want %d, nil", int64(a), bps, int64(got), err, int64(want))
		}
		if got < 0 || got > a {
			t.Fatalf("FeeBps(%d, %d) = %d: outside [0, amount]", int64(a), bps, int64(got))
		}
	}
	for _, a := range amounts {
		for _, bps := range rates {
			check(a, bps)
		}
	}
	for range 200_000 {
		check(Amount(rnd.Int64()), rnd.IntN(10_001))
		check(Amount(rnd.Int64N(1_000_000_000_000)), rnd.IntN(10_001))
	}
}

// Комиссия монотонна по сумме и ставке: больше сумма или ставка — не меньше комиссия.
func TestFeeBpsMonotonic(t *testing.T) {
	for _, bps := range []int{1, 50, 150, 5000, 9999} {
		var prev Amount
		for a := Amount(0); a < 30_000; a++ {
			got, err := FeeBps(a, bps)
			if err != nil {
				t.Fatal(err)
			}
			if got < prev {
				t.Fatalf("FeeBps(%d, %d) = %d < FeeBps(%d, %d) = %d", a, bps, got, a-1, bps, prev)
			}
			prev = got
		}
	}
}

// Недопустимые аргументы дают ошибку и нулевую комиссию, а не панику и не «какое-то» число.
func TestFeeBpsRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name   string
		amount Amount
		bps    int
	}{
		{"negative amount", -1, 150},
		{"min int64", math.MinInt64, 150},
		{"negative rate", 100, -1},
		{"rate above 100 percent", 100, 10_001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FeeBps(tt.amount, tt.bps)
			if got != 0 || !errors.Is(err, ErrInvalidFeeArgs) {
				t.Errorf("FeeBps(%d, %d) = %d, %v; want 0, ErrInvalidFeeArgs", int64(tt.amount), tt.bps, int64(got), err)
			}
		})
	}
}

func BenchmarkFeeBps(b *testing.B) {
	var sink Amount
	for i := 0; i < b.N; i++ {
		sink, _ = FeeBps(math.MaxInt64-Amount(i&0xff), 150)
	}
	_ = sink
}
