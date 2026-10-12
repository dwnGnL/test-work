package partners

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/dwnGnL/test-work/internal/ledger"
)

// maxFeeBps — 100% в базисных пунктах; то же ограничение стоит CHECK в базе.
const maxFeeBps = 10_000

// Partner — партнёр и его настройки. Сам API-ключ нигде не хранится и не возвращается.
type Partner struct {
	ID     int64
	Name   string
	FeeBps int // комиссия сервиса в базисных пунктах: 150 = 1,5%
}

// Create создаёт партнёра и его счёт в ledger. Оба шага идут в транзакции вызывающего:
// откатится она — исчезнут и партнёр, и счёт, партнёра без счёта не бывает.
// В базе лежит только sha256 от apiKey.
//
// Ошибки: ErrEmptyName, ErrEmptyAPIKey, ErrInvalidAPIKey, ErrInvalidFeeBps — проверки до
// обращения к базе;
// ErrAPIKeyTaken — ключ уже занят (в том числе параллельным вызовом).
func Create(ctx context.Context, tx pgx.Tx, name, apiKey string, feeBps int) (Partner, error) {
	switch {
	case strings.TrimSpace(name) == "":
		return Partner{}, ErrEmptyName
	case apiKey == "":
		return Partner{}, ErrEmptyAPIKey
	case strings.ContainsFunc(apiKey, isSpaceOrControl):
		return Partner{}, ErrInvalidAPIKey
	case feeBps < 0 || feeBps > maxFeeBps:
		return Partner{}, fmt.Errorf("%w: got %d", ErrInvalidFeeBps, feeBps)
	}

	p, inserted, err := insertPartner(ctx, tx, name, hashKey(apiKey), feeBps)
	if err != nil {
		return Partner{}, err
	}
	if !inserted {
		return Partner{}, ErrAPIKeyTaken
	}

	// Без счёта ledger.PartnerBalance вернул бы ErrUnknownAccount, а зачисление не нашло бы счёт.
	if err := ledger.EnsureAccount(ctx, tx, ledger.PartnerAccount(p.ID)); err != nil {
		return Partner{}, fmt.Errorf("partners: create ledger account for partner %d: %w", p.ID, err)
	}
	return p, nil
}

func isSpaceOrControl(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// Authenticate находит партнёра по API-ключу. Пустой ключ и неизвестный ключ дают
// ErrUnauthorized; пустой ключ отклоняется без обращения к базе.
func Authenticate(ctx context.Context, q Querier, apiKey string) (Partner, error) {
	if apiKey == "" {
		return Partner{}, ErrUnauthorized
	}
	p, found, err := selectByKeyHash(ctx, q, hashKey(apiKey))
	if err != nil {
		return Partner{}, err
	}
	if !found {
		return Partner{}, ErrUnauthorized
	}
	return p, nil
}

// hashKey возвращает sha256 ключа: ключ — случайная строка с высокой энтропией, а не пароль
// человека, поэтому медленный хэш с солью не нужен, а поиск по индексу работает.
func hashKey(apiKey string) []byte {
	sum := sha256.Sum256([]byte(apiKey))
	return sum[:]
}
