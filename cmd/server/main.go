// Command server — сервис приёма платежей в USDT.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dwnGnL/test-work/internal/chain"
	"github.com/dwnGnL/test-work/internal/config"
	"github.com/dwnGnL/test-work/internal/httpapi"
	"github.com/dwnGnL/test-work/internal/partners"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
	"github.com/dwnGnL/test-work/internal/scanner"
	"github.com/dwnGnL/test-work/migrations"
)

// shutdownTimeout — сколько ждём завершения текущих запросов при остановке.
const shutdownTimeout = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Контекст отменяется по SIGINT/SIGTERM — с него начинается корректная остановка.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool, migrations.FS, log); err != nil {
		return err
	}

	// Демо-партнёры с публичными ключами: только по явному флагу, не из миграции.
	if cfg.DemoSeed {
		if err := partners.SeedDemo(ctx, pool); err != nil {
			return err
		}
		log.Warn("demo partners with publicly known API keys are enabled (DEMO_SEED); do not use outside local runs",
			"names", "alpha, beta")
	}

	// Сеть — симулятор, её состояние лежит в базе. Сканер читает её через интерфейс chain.Chain.
	simulator := chain.NewSimulator(pool)
	opts := httpapi.Options{InvoiceTTL: cfg.InvoiceTTL, USDTContract: cfg.USDTContract}
	if cfg.SimAPI {
		opts.Simulator = simulator
		log.Warn("simulator control endpoints /sim/* are enabled without authentication (SIM_API); do not use outside local runs")
	}

	// Сканер блоков работает в фоне до остановки сервиса; перед выходом его дожидаются,
	// иначе проход мог бы оборваться на закрытом пуле.
	scanCtx, stopScan := context.WithCancel(ctx)
	var scanWG sync.WaitGroup
	scanWG.Add(1)
	go func() {
		defer scanWG.Done()
		scanLoop(scanCtx, scanner.New(pool, simulator, cfg.Confirmations, cfg.USDTContract, log), cfg.ScanInterval, log)
	}()
	defer func() {
		stopScan()
		scanWG.Wait()
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(pool, log, opts),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Пароль из DATABASE_URL в лог не пишем.
	log.Info("starting server",
		"addr", cfg.HTTPAddr,
		"confirmations", cfg.Confirmations,
		"invoice_ttl", cfg.InvoiceTTL.String(),
		"scan_interval", cfg.ScanInterval.String(),
		"sim_api", cfg.SimAPI,
		"usdt_contract", cfg.USDTContract,
		"aml_blacklist_size", len(cfg.AMLBlacklist))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		// Сервер упал сам (например, порт занят) — до сигнала.
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Контекст сигнала уже отменён, поэтому для остановки берём новый с таймаутом.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("server stopped")
	return nil
}

// scanLoop вызывает Tick сканера по таймеру, пока ctx не отменён. Ошибка прохода пишется в лог,
// сканер продолжает со следующего прохода: курсор в базе, потерять нечего. Отмена контекста при
// остановке сервиса ошибкой не считается.
func scanLoop(ctx context.Context, s *scanner.Scanner, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				log.Error("scanner tick failed", "err", err)
			}
		}
	}
}
