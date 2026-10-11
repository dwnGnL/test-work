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

// cleanupTimeout ограничивает откат транзакции, который выполняется уже после отмены ctx.
const cleanupTimeout = 5 * time.Second

// InTx выполняет fn в транзакции (уровень изоляции по умолчанию, READ COMMITTED).
// Транзакция фиксируется, только если fn вернула nil и Commit прошёл; в остальных
// случаях — ошибка fn, ошибка Commit, panic, runtime.Goexit в fn — её откатывает
// отложенный вызов. Откат идёт на контексте без отмены: если ctx уже отменён,
// соединение всё равно должно вернуться в пул в чистом состоянии.
//
// Ошибка Commit неоднозначна: если ctx отменён или соединение оборвалось во время
// COMMIT, сервер мог успеть зафиксировать транзакцию. Поэтому после такой ошибки
// повторять операцию можно только если она идемпотентна по ключу.
func InTx(ctx context.Context, db Beginner, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		// ErrTxClosed — транзакцию уже закрыл сам Commit (он закрывает её и при ошибке).
		// Ошибка fn первична; ошибку отката добавляем рядом, а не вместо неё.
		if rbErr := tx.Rollback(rctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback tx: %w", rbErr))
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}
