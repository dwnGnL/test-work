// Package config читает настройки сервиса из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Значения по умолчанию.
const (
	defaultDatabaseURL   = "postgres://payments:payments@localhost:5432/payments?sslmode=disable"
	defaultHTTPAddr      = ":8080"
	defaultConfirmations = 3
	defaultInvoiceTTL    = 60 * time.Minute
	// Адрес контракта USDT (ERC-20) в основной сети Ethereum.
	defaultUSDTContract = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
)

// Config — настройки сервиса.
type Config struct {
	// DatabaseURL — строка подключения к Postgres (DATABASE_URL).
	DatabaseURL string
	// HTTPAddr — адрес HTTP-сервера (HTTP_ADDR).
	HTTPAddr string
	// Confirmations — сколько подтверждений ждать до обработки платежа (CONFIRMATIONS), не меньше 1.
	Confirmations int
	// InvoiceTTL — срок жизни счёта (INVOICE_TTL, формат time.ParseDuration).
	InvoiceTTL time.Duration
	// AMLBlacklist — адреса отправителей с высоким риском для AML-мока
	// (AML_BLACKLIST, через запятую). Адреса приведены к нижнему регистру.
	AMLBlacklist []string
	// USDTContract — адрес контракта USDT (USDT_CONTRACT). Приведён к нижнему регистру:
	// регистр в адресе Ethereum несёт только контрольную сумму, сравнивать надо без него.
	USDTContract string
}

var addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Load читает конфигурацию из окружения процесса.
func Load() (Config, error) {
	return load(os.Getenv)
}

// load — Load с подставляемым источником переменных, чтобы тестировать без os.Setenv.
// Пустая переменная считается незаданной. Ошибки по всем переменным собираются вместе.
func load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	var errs []error
	cfg := Config{
		DatabaseURL: get("DATABASE_URL", defaultDatabaseURL),
		HTTPAddr:    get("HTTP_ADDR", defaultHTTPAddr),
	}

	cfg.Confirmations = defaultConfirmations
	if v := get("CONFIRMATIONS", ""); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("CONFIRMATIONS: %q is not an integer", v))
		case n < 1:
			errs = append(errs, fmt.Errorf("CONFIRMATIONS: must be >= 1, got %d", n))
		default:
			cfg.Confirmations = n
		}
	}

	cfg.InvoiceTTL = defaultInvoiceTTL
	if v := get("INVOICE_TTL", ""); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("INVOICE_TTL: %w", err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("INVOICE_TTL: must be positive, got %s", d))
		default:
			cfg.InvoiceTTL = d
		}
	}

	contract, err := normalizeAddress(get("USDT_CONTRACT", defaultUSDTContract))
	if err != nil {
		errs = append(errs, fmt.Errorf("USDT_CONTRACT: %w", err))
	}
	cfg.USDTContract = contract

	for _, raw := range strings.Split(getenv("AML_BLACKLIST"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := normalizeAddress(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("AML_BLACKLIST: %w", err))
			continue
		}
		cfg.AMLBlacklist = append(cfg.AMLBlacklist, addr)
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// normalizeAddress проверяет формат адреса Ethereum (0x и 40 hex-символов)
// и приводит его к нижнему регистру.
func normalizeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !addressRe.MatchString(s) {
		return "", fmt.Errorf("%q is not an Ethereum address (0x + 40 hex digits)", s)
	}
	return strings.ToLower(s), nil
}
