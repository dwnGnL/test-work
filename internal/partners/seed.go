package partners

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwnGnL/test-work/internal/platform/postgres"
)

// demoPartner — демо-партнёр. Ключи публичны, поэтому SeedDemo вызывают только под флагом DEMO_SEED.
type demoPartner struct {
	name   string
	apiKey string
	feeBps int
}

var demoPartners = []demoPartner{
	{name: "alpha", apiKey: "demo-key-alpha", feeBps: 150},
	{name: "beta", apiKey: "demo-key-beta", feeBps: 50},
}

// SeedDemo создаёт двух демо-партнёров со счетами в ledger, если их ещё нет. Повтор и
// параллельные вызовы безопасны: партнёры отличаются ключом, а он уникален в базе, поэтому
// проигравший в гонке ждёт коммита победителя и пропускает уже созданного. Все вызовы
// идут в одном порядке, так что взаимной блокировки нет.
func SeedDemo(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		for _, d := range demoPartners {
			_, err := Create(ctx, tx, d.name, d.apiKey, d.feeBps)
			if errors.Is(err, ErrAPIKeyTaken) {
				continue // уже есть
			}
			if err != nil {
				return fmt.Errorf("partners: seed %q: %w", d.name, err)
			}
		}
		return nil
	})
}
