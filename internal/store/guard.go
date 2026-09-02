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

// SetWriteMode installs or removes the guard for a tracked table (§6.3).
func (s *Store) SetWriteMode(ctx context.Context, t *Table, mode WriteMode) error {
	var g adapter.GuardMode
	switch mode {
	case ModeOpen:
		g = adapter.GuardOpen
	case ModeGuarded:
		g = adapter.GuardReject
	case ModeCapture:
		g = adapter.GuardCapture
	default:
		return fmt.Errorf("unknown write mode %q", mode)
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return s.ad.InstallGuard(ctx, tx, t.Physical, g)
	})
}

// MarkWriter sets the guard marker so DataGit's own writes pass a guarded
// table's trigger.
func (s *Store) MarkWriter(ctx context.Context, tx adapter.Tx) error {
	return s.ad.MarkWriter(ctx, tx)
}

// DriftEvents counts observed out-of-band writes in capture mode.
func (s *Store) DriftEvents(ctx context.Context, t *Table) (int, error) {
	var n int
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT count(*) FROM datagit_drift_log WHERE table_name = $1`, t.Physical).Scan(&n)
	return n, err
}

var _ = catalog.SidecarTable
