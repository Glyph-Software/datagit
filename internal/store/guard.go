package store

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
)

// WriteMode controls what happens when something writes to a tracked live table
// without going through DataGit (§6.3).
//
// The `T ≡ main@HEAD` invariant holds only while writes go through DataGit, and
// something eventually will not: a psql session, a legacy job, a migration tool.
type WriteMode string

const (
	// ModeOpen adds nothing. Drift is possible and undetected until a
	// verification scan. Zero cost, and the default.
	ModeOpen WriteMode = "open"

	// ModeGuarded rejects writes that lack DataGit's session marker.
	//
	// It stops accidents, not adversaries: the marker is a connection-level
	// variable that any client with write access can set. A seatbelt, not a lock,
	// and the §12.2 caveat about privileged operators applies here too.
	ModeGuarded WriteMode = "guarded"

	// ModeCapture records out-of-band writes into the sidecar as `external`
	// commits.
	//
	// This is trigger-based CDC, which fires synchronously on every write and
	// adds latency and write amplification to the source table — precisely the
	// cost DataGit's API-first write path avoids. It is a compatibility bridge
	// for tables with legacy writers, not a recommended steady state.
	ModeCapture WriteMode = "capture"
)

// GuardMarker is the connection-level variable DataGit sets inside its own
// transactions so the guard trigger can recognize them.
const GuardMarker = "datagit.writer"

// SetWriteMode installs or removes the trigger for a tracked table (§6.3).
func (s *Store) SetWriteMode(ctx context.Context, t *Table, mode WriteMode) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		trg := "datagit_guard_" + t.Physical
		fn := "datagit_guard_fn_" + t.Physical

		if err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`,
			quote(trg), quote(t.Physical))); err != nil {
			return err
		}
		if err := tx.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, quote(fn))); err != nil {
			return err
		}
		if mode == ModeOpen {
			return nil
		}

		var body string
		switch mode {
		case ModeGuarded:
			body = fmt.Sprintf(`
				IF current_setting('%s', true) IS DISTINCT FROM 'yes' THEN
					RAISE EXCEPTION
						'table %%s is tracked by DataGit in guarded mode: write through the '
						'DataGit API, or set the table to open mode to allow direct writes',
						TG_TABLE_NAME;
				END IF;
				RETURN COALESCE(NEW, OLD);`, GuardMarker)
		case ModeCapture:
			// Capture records the fact of an out-of-band write. Reconstructing a
			// full version from inside a trigger would need the commit machinery,
			// so this marks the table as drifted and leaves the reconciliation to
			// `verify --drift`, which is honest about what a trigger can know: it
			// has no author, no message, and no commit boundary.
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
			 BEGIN %s END $fn$`, quote(fn), body)); err != nil {
			return fmt.Errorf("create guard function: %w", err)
		}
		return tx.Exec(ctx, fmt.Sprintf(
			`CREATE TRIGGER %s BEFORE INSERT OR UPDATE OR DELETE ON %s
			 FOR EACH ROW EXECUTE FUNCTION %s()`,
			quote(trg), quote(t.Physical), quote(fn)))
	})
}

// MarkWriter sets the guard marker for the current transaction, so DataGit's own
// writes pass a guarded table's trigger.
func MarkWriter(ctx context.Context, tx adapter.Tx) error {
	return tx.Exec(ctx, fmt.Sprintf(`SELECT set_config('%s', 'yes', true)`, GuardMarker))
}

// DriftEvents counts observed out-of-band writes in capture mode.
func (s *Store) DriftEvents(ctx context.Context, t *Table) (int, error) {
	var n int
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT count(*) FROM datagit_drift_log WHERE table_name = $1`, t.Physical).Scan(&n)
	return n, err
}

var _ = catalog.SidecarTable
