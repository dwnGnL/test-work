package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateLockKey — ключ advisory-блокировки, общий для всех инстансов сервиса.
// Значение произвольное, важно лишь, чтобы его не занимал никто другой в этой базе.
const migrateLockKey int64 = 0x7573_6474_6d69_6772 // "usdtmigr"

type migration struct {
	version string // имя файла без .sql, например 0001_partners
	sql     string
}

// Migrate применяет к базе все ещё не применённые миграции из fsys.
//
// Файлы *.sql в корне fsys идут по порядку имён; применённые версии записываются
// в public.schema_migrations. Каждая миграция выполняется в своей транзакции вместе
// с записью о ней: после сбоя в базе либо вся миграция, либо ничего. На время работы
// берётся advisory-блокировка, поэтому два инстанса, стартующих одновременно, не
// мигрируют параллельно: второй дожидается первого и видит уже применённые версии.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) (err error) {
	migrations, err := readMigrations(fsys)
	if err != nil {
		return err
	}

	// Блокировка привязана к сессии, поэтому всё делаем на одном соединении.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Разблокируем на контексте без отмены: иначе при отмене ctx блокировка
		// осталась бы за соединением, вернувшимся в пул.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		var unlocked bool
		uerr := conn.QueryRow(uctx, "SELECT pg_advisory_unlock($1)", migrateLockKey).Scan(&unlocked)
		if uerr == nil && unlocked {
			return
		}
		// Состояние блокировки неизвестно: закрываем сессию, это снимает блокировку.
		_ = conn.Hijack().Close(uctx)
		if err == nil {
			err = fmt.Errorf("release migration lock: %v (unlocked=%t)", uerr, unlocked)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public.schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		err := InTx(ctx, conn, func(tx pgx.Tx) error {
			// Без аргументов pgx использует simple protocol, и в файле может быть много операторов.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO public.schema_migrations (version) VALUES ($1)", m.version)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", m.version, err)
		}
		log.Info("migration applied", "version", m.version)
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM public.schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// readMigrations читает *.sql из корня fsys и сортирует по имени.
func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: strings.TrimSuffix(e.Name(), ".sql"), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// compile-time проверка: пул и соединение пригодны для InTx.
var (
	_ Beginner = (*pgxpool.Pool)(nil)
	_ Beginner = (*pgxpool.Conn)(nil)
)
