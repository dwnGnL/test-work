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
// в public.schema_migrations. Весь прогон — одна транзакция, и первым делом она берёт
// pg_advisory_xact_lock: два инстанса, стартующих одновременно, не мигрируют параллельно,
// второй дожидается коммита первого и видит уже применённые версии. Блокировка
// транзакционная, поэтому снимается сама на коммите или откате (и при обрыве соединения),
// отдельно разблокировать и привязывать работу к одному соединению не нужно.
//
// Цена единой транзакции: сбой любой миграции откатывает весь прогон, в том числе
// миграции, которые в нём уже выполнились (на пустой базе не останется даже
// schema_migrations). В базе остаются только версии, применённые прошлыми запусками.
// Для миграций из этого репозитория это приемлемо: после исправления следующий старт
// просто повторит прогон. Операторы, которые нельзя выполнять в транзакции
// (например, CREATE INDEX CONCURRENTLY), в такие файлы класть нельзя.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	migrations, err := readMigrations(fsys)
	if err != nil {
		return err
	}

	var applied []string
	err = InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}

		// Под блокировкой: параллельные CREATE TABLE IF NOT EXISTS друг с другом не конфликтуют.
		if _, err := tx.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS public.schema_migrations (
				version    TEXT PRIMARY KEY,
				applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
			)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}

		done, err := appliedVersions(ctx, tx)
		if err != nil {
			return err
		}

		for _, m := range migrations {
			if done[m.version] {
				continue
			}
			// Без аргументов pgx использует simple protocol, и в файле может быть много операторов.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("apply migration %s: %w", m.version, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO public.schema_migrations (version) VALUES ($1)", m.version); err != nil {
				return fmt.Errorf("apply migration %s: record version: %w", m.version, err)
			}
			applied = append(applied, m.version)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Пишем в лог только после коммита: до него «применено» ещё не гарантировано.
	for _, v := range applied {
		log.Info("migration applied", "version", v)
	}
	return nil
}

func appliedVersions(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx, "SELECT version FROM public.schema_migrations")
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
