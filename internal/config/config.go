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
	// minInvoiceTTL — нижняя граница срока счёта. Срок короче секунды не имеет смысла, а срок
	// меньше микросекунды округлился бы в базе до нуля: счёт рождался бы уже просроченным.
	minInvoiceTTL = time.Second
	// defaultScanInterval — пауза между проходами сканера блоков; minScanInterval — нижняя граница,
	// чтобы сканер не засыпал сеть и базу запросами.
	defaultScanInterval = time.Second
	minScanInterval     = 100 * time.Millisecond
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
	// ScanInterval — пауза между проходами сканера блоков (SCAN_INTERVAL, формат time.ParseDuration,
	// не меньше 100ms).
	ScanInterval time.Duration
	// SimAPI — включать ли ручки управления симулятором сети /sim/* (SIM_API, по умолчанию false).
	// Ручки открыты без входа и меняют «сеть», поэтому включают их только для проверки сценария.
	SimAPI bool
	// DemoSeed — создавать ли демо-партнёров при старте (DEMO_SEED, по умолчанию false).
	// Их API-ключи публичны, поэтому включают только для разработки и проверки сценария.
	DemoSeed bool
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
		case d < minInvoiceTTL:
			errs = append(errs, fmt.Errorf("INVOICE_TTL: must be at least %s, got %s", minInvoiceTTL, d))
		default:
			cfg.InvoiceTTL = d
		}
	}

	cfg.ScanInterval = defaultScanInterval
	if v := get("SCAN_INTERVAL", ""); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("SCAN_INTERVAL: %w", err))
		case d < minScanInterval:
			errs = append(errs, fmt.Errorf("SCAN_INTERVAL: must be at least %s, got %s", minScanInterval, d))
		default:
			cfg.ScanInterval = d
		}
	}

	if v := get("SIM_API", ""); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("SIM_API: %q is not a boolean", v))
		}
		cfg.SimAPI = b
	}

	if v := get("DEMO_SEED", ""); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("DEMO_SEED: %q is not a boolean", v))
		}
		cfg.DemoSeed = b
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
