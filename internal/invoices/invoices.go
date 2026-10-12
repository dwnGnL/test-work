package invoices

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/dwnGnL/test-work/internal/money"
)

const (
	// maxKeyLen — наибольшая длина ключа идемпотентности в символах.
	maxKeyLen = 255
	// maxFeeBps — 100% в базисных пунктах; то же ограничение стоит CHECK в базе.
	maxFeeBps = 10_000
	// addressBytes — длина адреса Ethereum в байтах.
	addressBytes = 20
)

// Invoice — счёт на оплату. Статус не хранится, его выводит Status.
type Invoice struct {
	ID             string // UUID в текстовом виде
	PartnerID      int64
	Amount         money.Amount
	FeeBps         int // ставка, зафиксированная при создании счёта
	DepositAddress string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

// CreateParams — параметры создания счёта.
type CreateParams struct {
	PartnerID      int64
	IdempotencyKey string // уникален в паре с PartnerID
	Amount         money.Amount
	FeeBps         int           // ставка партнёра, её передаёт вызывающий; копируется в счёт
	TTL            time.Duration // срок жизни счёта
}

// Create создаёт счёт, идемпотентно по (PartnerID, IdempotencyKey).
//
// Первый вызов вставляет счёт и возвращает created = true. Повтор с тем же ключом и тем же
// запросом возвращает исходный счёт и created = false, ничего не записывая. Тот же ключ
// с другим запросом — ErrIdempotencyConflict. В «запрос» входит то, что прислал партнёр
// (сумма); ставка и TTL приходят с нашей стороны и в сравнение не входят, поэтому смена
// ставки или настройки TTL между повторами не ломает повтор.
//
// Параметры проверяются до обращения к базе (ErrInvalid..., ErrNonPositive...).
// db — пул или транзакция; при гонке за один ключ проигравший ждёт коммита победителя, а
// затем читает его счёт отдельным запросом, поэтому нужен READ COMMITTED (по умолчанию).
func Create(ctx context.Context, db Querier, p CreateParams) (inv Invoice, created bool, err error) {
	if err := p.validate(); err != nil {
		return Invoice{}, false, err
	}
	address, err := newDepositAddress()
	if err != nil {
		return Invoice{}, false, err
	}
	hash := requestHash(p.Amount)

	inv, inserted, err := insertInvoice(ctx, db, p, hash, address)
	if err != nil {
		return Invoice{}, false, err
	}
	if inserted {
		return inv, true, nil
	}

	// Ключ уже занят: читаем сохранённый счёт и сверяем запрос.
	stored, storedHash, found, err := selectByKey(ctx, db, p.PartnerID, p.IdempotencyKey)
	if err != nil {
		return Invoice{}, false, err
	}
	if !found {
		// Вставка упёрлась в этот ключ, строка обязана быть видна.
		return Invoice{}, false, fmt.Errorf("invoices: idempotent invoice %q of partner %d disappeared", p.IdempotencyKey, p.PartnerID)
	}
	if !bytes.Equal(storedHash, hash) {
		return Invoice{}, false, ErrIdempotencyConflict
	}
	return stored, false, nil
}

// Get возвращает счёт партнёра. Чужой счёт, несуществующий счёт и id не в формате UUID
// дают одну и ту же ErrNotFound: по ответу нельзя узнать, существует ли чужой счёт.
func Get(ctx context.Context, q Querier, partnerID int64, id string) (Invoice, error) {
	if !isUUID(id) {
		return Invoice{}, ErrNotFound
	}
	inv, found, err := selectByID(ctx, q, partnerID, id)
	if err != nil {
		return Invoice{}, err
	}
	if !found {
		return Invoice{}, ErrNotFound
	}
	return inv, nil
}

// validate проверяет параметры, не обращаясь к базе.
func (p CreateParams) validate() error {
	switch {
	case p.PartnerID <= 0:
		return fmt.Errorf("%w: got %d", ErrInvalidPartnerID, p.PartnerID)
	case p.IdempotencyKey == "" || !utf8.ValidString(p.IdempotencyKey) ||
		utf8.RuneCountInString(p.IdempotencyKey) > maxKeyLen:
		// Не UTF-8 база не сохранит (ошибка вставки), поэтому отклоняем заранее.
		return ErrInvalidIdempotencyKey
	case p.Amount <= 0:
		return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, p.Amount)
	case p.FeeBps < 0 || p.FeeBps > maxFeeBps:
		return fmt.Errorf("%w: got %d", ErrInvalidFeeBps, p.FeeBps)
	case p.TTL < time.Microsecond:
		// База хранит время с точностью до микросекунды: меньший срок округлился бы до нуля.
		return fmt.Errorf("%w: got %s", ErrNonPositiveTTL, p.TTL)
	}
	return nil
}

// requestHash — sha256 канонического вида запроса: сумма в минимальных единицах. Хэш берётся
// не от сырого тела, поэтому пробелы и порядок полей в JSON, а также "100" и "100.000000"
// не делают запрос «другим». Новое поле запроса добавлять в хэш, только когда оно задано:
// иначе хэши уже созданных счетов изменятся и их повтор начнёт получать конфликт.
func requestHash(amount money.Amount) []byte {
	sum := sha256.Sum256([]byte("amount=" + strconv.FormatInt(int64(amount), 10)))
	return sum[:]
}

// newDepositAddress возвращает случайный адрес: 0x и 40 hex-символов в нижнем регистре.
// Ключей к нему нет: реальные адреса и ключи по условию не нужны. Для 20 случайных байт
// совпадение с чужим адресом исключено, а UNIQUE в базе всё равно не даст сохранить дубль.
func newDepositAddress() (string, error) {
	var b [addressBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("invoices: generate deposit address: %w", err)
	}
	return "0x" + hex.EncodeToString(b[:]), nil
}

// isUUID проверяет запись UUID вида 8-4-4-4-12 из hex-цифр. База принимает и другие записи
// (в скобках, без дефисов), но id в API у нас только такой.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}
