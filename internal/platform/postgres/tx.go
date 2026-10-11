package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Beginner — всё, что умеет начинать транзакцию: *pgxpool.Pool и *pgxpool.Conn.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// cleanupTimeout ограничивает откат транзакции и снятие блокировки, которые
// выполняются уже после отмены ctx.
const cleanupTimeout = 5 * time.Second

// InTx выполняет fn в транзакции (уровень изоляции по умолчанию, READ COMMITTED).
// Ошибка или паника в fn откатывают транзакцию, иначе выполняется коммит.
// Откат идёт на контексте без отмены: если ctx уже отменён, соединение
// всё равно должно вернуться в пул в чистом состоянии.
func InTx(ctx context.Context, db Beginner, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	rollback := func() error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := tx.Rollback(rctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return fmt.Errorf("rollback tx: %w", err)
		}
		return nil
	}

	defer func() {
		if p := recover(); p != nil {
			_ = rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		// Ошибка fn первична; ошибку отката добавляем рядом, а не вместо неё.
		if rbErr := rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
