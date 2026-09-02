package postgres

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// ApplyMigration runs a plan through the resumable journalled state machine
// (§10.4).
//
// PostgreSQL has transactional DDL and could wrap this in one transaction.
// It deliberately does not: MySQL cannot, and running the SAME machine on both
// means failure behaviour is identical and only has to be tested once. S4
// verified convergence from every crash point on both engines.
//
// The contract each operation must honour: it is journalled as started BEFORE
// execution, marked complete after, and written to be idempotent — because a
// resume re-runs whatever was in flight when the process died.
func (a *Adapter) ApplyMigration(ctx context.Context, plan *adapter.MigrationPlan, j adapter.Journal) error {
	if err := j.Begin(ctx, plan); err != nil {
		return fmt.Errorf("journal begin: %w", err)
	}
	done, err := j.Completed(ctx, plan.TableID)
	if err != nil {
		return fmt.Errorf("journal read: %w", err)
	}
	for _, op := range plan.Ops {
		if done[op.Ordinal] {
			continue // already applied; a resume must not redo it
		}
		if err := j.MarkStarted(ctx, plan.TableID, op.Ordinal); err != nil {
			return fmt.Errorf("journal start op %d: %w", op.Ordinal, err)
		}
		if err := a.execMigrationOp(ctx, op); err != nil {
			// The journal keeps this operation marked started-but-not-complete, so
			// a restart re-runs exactly this one and nothing before it.
			return fmt.Errorf("migration op %d (%s): %w", op.Ordinal, op.Kind, err)
		}
		if err := j.MarkComplete(ctx, plan.TableID, op.Ordinal); err != nil {
			return fmt.Errorf("journal complete op %d: %w", op.Ordinal, err)
		}
	}
	return nil
}

// execMigrationOp runs one operation outside any transaction, which is what
// makes the journal necessary in the first place.
func (a *Adapter) execMigrationOp(ctx context.Context, op adapter.MigrationOp) error {
	if a.exec == nil {
		return fmt.Errorf("postgres: adapter has no execution handle; use NewWithExec")
	}
	return a.exec(ctx, op.SQL)
}
