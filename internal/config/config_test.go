package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL:   defaultDatabaseURL,
		HTTPAddr:      ":8080",
		Confirmations: 3,
		InvoiceTTL:    60 * time.Minute,
		ScanInterval:  time.Second,
		SimAPI:        false,
		AMLBlacklist:  nil,
		DemoSeed:      false,
		USDTContract:  "0xdac17f958d2ee523a2206206994597c13d831ec7",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"DATABASE_URL":  "postgres://u:p@db:5432/x",
		"HTTP_ADDR":     "127.0.0.1:9000",
		"CONFIRMATIONS": "12",
		"INVOICE_TTL":   "15m",
		"SCAN_INTERVAL": "250ms",
		"SIM_API":       "true",
		"DEMO_SEED":     "true",
		"AML_BLACKLIST": " 0xAAAAaaaaAAAAaaaaAAAAaaaaAAAAaaaaAAAAaaaa , ,0x000000000000000000000000000000000000dEaD,",
		"USDT_CONTRACT": "0x1111111111111111111111111111111111111111",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL:   "postgres://u:p@db:5432/x",
		HTTPAddr:      "127.0.0.1:9000",
		Confirmations: 12,
		InvoiceTTL:    15 * time.Minute,
		ScanInterval:  250 * time.Millisecond,
		SimAPI:        true,
		AMLBlacklist: []string{
			"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"0x000000000000000000000000000000000000dead",
		},
		DemoSeed:     true,
		USDTContract: "0x1111111111111111111111111111111111111111",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadEmptyVarsUseDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{"HTTP_ADDR": "  ", "CONFIRMATIONS": "", "INVOICE_TTL": "", "DEMO_SEED": " ", "SCAN_INTERVAL": " ", "SIM_API": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Confirmations != 3 || cfg.InvoiceTTL != time.Hour || cfg.DemoSeed ||
		cfg.ScanInterval != time.Second || cfg.SimAPI {
		t.Errorf("empty variables must fall back to defaults, got %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // подстрока в тексте ошибки
	}{
		{"confirmations not a number", map[string]string{"CONFIRMATIONS": "three"}, "CONFIRMATIONS"},
		{"confirmations zero", map[string]string{"CONFIRMATIONS": "0"}, "CONFIRMATIONS"},
		{"confirmations negative", map[string]string{"CONFIRMATIONS": "-2"}, "CONFIRMATIONS"},
		{"ttl garbage", map[string]string{"INVOICE_TTL": "soon"}, "INVOICE_TTL"},
		{"ttl without unit", map[string]string{"INVOICE_TTL": "60"}, "INVOICE_TTL"},
		{"ttl zero", map[string]string{"INVOICE_TTL": "0s"}, "INVOICE_TTL"},
		{"ttl negative", map[string]string{"INVOICE_TTL": "-5m"}, "INVOICE_TTL"},
		{"ttl below a second", map[string]string{"INVOICE_TTL": "999ms"}, "INVOICE_TTL"},
		{"ttl of one nanosecond", map[string]string{"INVOICE_TTL": "1ns"}, "INVOICE_TTL"},
		{"contract too short", map[string]string{"USDT_CONTRACT": "0x1234"}, "USDT_CONTRACT"},
		{"contract no prefix", map[string]string{"USDT_CONTRACT": "dAC17F958D2ee523a2206206994597C13D831ec7"}, "USDT_CONTRACT"},
		{"scan interval garbage", map[string]string{"SCAN_INTERVAL": "often"}, "SCAN_INTERVAL"},
		{"scan interval without unit", map[string]string{"SCAN_INTERVAL": "1"}, "SCAN_INTERVAL"},
		{"scan interval zero", map[string]string{"SCAN_INTERVAL": "0s"}, "SCAN_INTERVAL"},
		{"scan interval negative", map[string]string{"SCAN_INTERVAL": "-1s"}, "SCAN_INTERVAL"},
		{"scan interval below the minimum", map[string]string{"SCAN_INTERVAL": "99ms"}, "SCAN_INTERVAL"},
		{"sim api not a boolean", map[string]string{"SIM_API": "sometimes"}, "SIM_API"},
		{"demo seed not a boolean", map[string]string{"DEMO_SEED": "maybe"}, "DEMO_SEED"},
		{"blacklist bad entry", map[string]string{"AML_BLACKLIST": "0x1111111111111111111111111111111111111111,nope"}, "AML_BLACKLIST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(env(tt.env))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %s", err, tt.want)
			}
		})
	}
}

// Граница SCAN_INTERVAL включена: ровно 100ms допустимо.
func TestLoadScanIntervalBoundary(t *testing.T) {
	cfg, err := load(env(map[string]string{"SCAN_INTERVAL": "100ms"}))
	if err != nil || cfg.ScanInterval != 100*time.Millisecond {
		t.Errorf("SCAN_INTERVAL=100ms: got %v, %v", cfg.ScanInterval, err)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := load(env(map[string]string{"CONFIRMATIONS": "x", "INVOICE_TTL": "y"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{"CONFIRMATIONS", "INVOICE_TTL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q must mention %s", err, key)
		}
	}
}
