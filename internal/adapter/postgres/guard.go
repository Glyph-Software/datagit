package postgres

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// GuardMarker is the setting DataGit's own transactions carry (§6.3).
const GuardMarker = "datagit.writer"

// MarkWriter sets the marker for the current TRANSACTION.
//
// The third argument to set_config is is_local: the setting is discarded at
// commit or rollback. A session-scoped marker would outlive the transaction and
// leave a pooled connection permanently trusted.
func (a *Adapter) MarkWriter(ctx context.Context, tx adapter.Tx) error {
	return tx.Exec(ctx, fmt.Sprintf(`SELECT set_config('%s', 'yes', true)`, GuardMarker))
}

// InstallGuard installs a row trigger on the live table.
//
// This is the ONE path that touches a tracked live table, and it is off by
// default for that reason (§5.1, invariant 1). Nothing here runs on the happy
// path.
func (a *Adapter) InstallGuard(ctx context.Context, tx adapter.Tx, physical string,
	mode adapter.GuardMode) error {

	trg, fn := "datagit_guard_"+physical, "datagit_guard_fn_"+physical

	if err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`,
		quoteIdent(trg), quoteIdent(physical))); err != nil {
		return err
	}
	if err := tx.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, quoteIdent(fn))); err != nil {
		return err
	}
	if mode == adapter.GuardOpen {
		return nil
	}

	var body string
	switch mode {
	case adapter.GuardReject:
		body = fmt.Sprintf(`
			IF current_setting('%s', true) IS DISTINCT FROM 'yes' THEN
				RAISE EXCEPTION
					'table %%s is tracked by DataGit in guarded mode: write through the '
					'DataGit API, or set the table to open mode to allow direct writes',
					TG_TABLE_NAME;
			END IF;
			RETURN COALESCE(NEW, OLD);`, GuardMarker)
	case adapter.GuardCapture:
		// Capture records the FACT of an out-of-band write. Reconstructing a full
		// version from inside a trigger would need the commit machinery, so this
		// marks the table as drifted and leaves reconciliation to `verify --drift`,
		// which is honest about what a trigger can know: no author, no message, no
		// commit boundary.
		body = fmt.Sprintf(`
			IF current_setting('%s', true) IS DISTINCT FROM 'yes' THEN
				INSERT INTO datagit_drift_log (table_name, op, observed_at)
				VALUES (TG_TABLE_NAME, TG_OP, now());
			END IF;
			RETURN COALESCE(NEW, OLD);`, GuardMarker)
	default:
		return fmt.Errorf("unknown write mode %q", mode)
	}

	if err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
		 BEGIN %s END $fn$`, quoteIdent(fn), body)); err != nil {
		return fmt.Errorf("create guard function: %w", err)
	}
	return tx.Exec(ctx, fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE INSERT OR UPDATE OR DELETE ON %s
		 FOR EACH ROW EXECUTE FUNCTION %s()`,
		quoteIdent(trg), quoteIdent(physical), quoteIdent(fn)))
}
