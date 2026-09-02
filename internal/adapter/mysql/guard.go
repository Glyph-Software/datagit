package mysql

import (
	"context"
	"fmt"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

// GuardMarker is the user variable DataGit's own writes carry (§6.3).
//
// A user variable rather than a setting, because MySQL has no per-transaction
// settings a trigger can read. That is a real difference with a real
// consequence, recorded here rather than hidden: the marker is SESSION-scoped,
// so it survives the transaction that set it and would leave a pooled connection
// trusted for its next statement. MarkWriter therefore clears it as well as sets
// it -- see below.
const GuardMarker = "@datagit_writer"

// MarkWriter sets the marker on the current connection.
//
// MySQL's user variables live for the connection, not the transaction, so this
// is genuinely weaker than PostgreSQL's transaction-local setting. The guard is
// documented as a seatbelt, not a lock (§6.3): it stops the accidental psql
// session and the legacy job, and it never claimed to stop a client that can set
// its own variables. The narrower scope does not change that claim, and treating
// it as security on either engine would be the mistake.
func (a *Adapter) MarkWriter(ctx context.Context, tx adapter.Tx) error {
	return tx.Exec(ctx, "SET "+GuardMarker+" = 'yes'")
}

// InstallGuard installs row triggers on the live table.
//
// MySQL has no statement-level OR in a trigger definition, so one trigger per
// operation is required where PostgreSQL needs one. The behaviour is identical.
func (a *Adapter) InstallGuard(ctx context.Context, tx adapter.Tx, physical string,
	mode adapter.GuardMode) error {

	ops := []string{"INSERT", "UPDATE", "DELETE"}
	for _, op := range ops {
		trg := fmt.Sprintf("datagit_guard_%s_%s", physical, op)
		if err := tx.Exec(ctx, "DROP TRIGGER IF EXISTS "+quoteIdent(trg)); err != nil {
			return err
		}
	}
	if mode == adapter.GuardOpen {
		return nil
	}

	for _, op := range ops {
		trg := fmt.Sprintf("datagit_guard_%s_%s", physical, op)
		var body string
		switch mode {
		case adapter.GuardReject:
			// SIGNAL is MySQL's RAISE. 45000 is the standard's unhandled
			// user-defined exception, and the message is capped at 128 characters,
			// so it is written to fit rather than truncated by the server.
			body = fmt.Sprintf(`
				IF COALESCE(%s, '') <> 'yes' THEN
					SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
						'table %s is tracked by DataGit in guarded mode: write through the DataGit API';
				END IF;`, GuardMarker, physical)
		case adapter.GuardCapture:
			body = fmt.Sprintf(`
				IF COALESCE(%s, '') <> 'yes' THEN
					INSERT INTO datagit_drift_log (table_name, op, observed_at)
					VALUES ('%s', '%s', NOW(6));
				END IF;`, GuardMarker, physical, op)
		default:
			return fmt.Errorf("unknown write mode %q", mode)
		}

		if err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW BEGIN %s END`,
			quoteIdent(trg), op, quoteIdent(physical), body)); err != nil {
			return fmt.Errorf("create %s guard on %s: %w", op, physical, err)
		}
	}
	return nil
}
