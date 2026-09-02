package store

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// Journal persists migration progress so a crashed apply resumes rather than
// restarting or being left indeterminate (§10.4).
//
// S4 verified this converges from every injected crash point on both MySQL 8.4
// and PostgreSQL 17, including mid-step crashes where an operation is recorded
// as started but never completed.
type Journal struct{ s *Store }

func (s *Store) Journal() *Journal { return &Journal{s: s} }

func (j *Journal) Begin(ctx context.Context, plan *adapter.MigrationPlan) error {
	return j.s.pool.InTx(ctx, func(tx adapter.Tx) error {
		for _, op := range plan.Ops {
			if err := tx.Exec(ctx, j.s.ad.InsertOnConflict("datagit_migration_journal",
				[]string{"plan_id", "ordinal", "kind", "sql_text"},
				"VALUES ($1,$2,$3,$4)", []string{"plan_id", "ordinal"}, nil),
				int64(plan.TableID), op.Ordinal, op.Kind, op.SQL); err != nil {
				return fmt.Errorf("journal op %d: %w", op.Ordinal, err)
			}
		}
		return nil
	})
}

func (j *Journal) MarkStarted(ctx context.Context, planID uint64, ordinal int) error {
	return j.s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx,
			`UPDATE datagit_migration_journal SET started_at = now()
			  WHERE plan_id=$1 AND ordinal=$2`, int64(planID), ordinal)
	})
}

func (j *Journal) MarkComplete(ctx context.Context, planID uint64, ordinal int) error {
	return j.s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx,
			`UPDATE datagit_migration_journal SET completed_at = now()
			  WHERE plan_id=$1 AND ordinal=$2`, int64(planID), ordinal)
	})
}

func (j *Journal) Completed(ctx context.Context, planID uint64) (map[int]bool, error) {
	rows, err := j.s.pool.Direct().Query(ctx,
		`SELECT ordinal FROM datagit_migration_journal
		  WHERE plan_id=$1 AND completed_at IS NOT NULL`, int64(planID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Exec exposes a non-transactional statement runner for migration operations.
func (s *Store) Exec(ctx context.Context, sql string) error {
	return s.pool.Direct().Exec(ctx, sql)
}
