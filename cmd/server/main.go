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
	"syscall"
	"time"

	"github.com/dwnGnL/test-work/internal/config"
	"github.com/dwnGnL/test-work/internal/platform/postgres"
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

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthHandler(pool, log))

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
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
