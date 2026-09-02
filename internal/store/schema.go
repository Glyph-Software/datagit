package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
	"github.com/Glyph-Software/datagit/internal/schemaeng"
)

// Schema changes flow branch → proposal → plan → apply → live table (§10.4).
//
// The shape of that flow is forced by the one fixed decision the whole design
// rests on: applications read the live tables DIRECTLY. A data merge into main
// can apply immediately because a row's new value is still a row. A schema merge
// cannot, because dropping a column out from under a compiled query breaks every
// direct reader at once, with no warning and no way to sequence a deploy around
// it.
//
// So a schema merge produces a MIGRATION PLAN that someone applies deliberately.
// That is the deliberate un-Git-like part of DataGit, and it is the price of
// main reads never touching the service.

// LoadSchema returns a branch's current schema version (§10.1).
//
// A branch with no schema version of its own inherits its parent's, walking to
// the repository default. That is what makes branching a schema free: a branch
// that never changes shape stores nothing.
func (s *Store) LoadSchema(ctx context.Context, repo *Repo, t *Table, branch string) (
	*schemaeng.Version, error) {

	branchID, _, _, chain, err := s.loadRef(ctx, s.pool.Direct(), repo, branch)
	if err != nil {
		return nil, err
	}
	// The chain already carries the branch itself at index 0 and then its
	// ancestry, so it is exactly the lookup order a schema inherits along.
	ids := []uuid.UUID{branchID}
	for _, seg := range chain {
		ids = append(ids, uuid.UUID(seg.BranchID))
	}
	for _, id := range ids {
		v, err := s.loadSchemaAt(ctx, t, id, -1)
		if err != nil {
			return nil, err
		}
		if v != nil {
			return v, nil
		}
	}
	// No stored version anywhere: the table's introspected shape at track time is
	// epoch 0. Nothing has changed shape yet, so there is nothing to store.
	return &schemaeng.Version{
		TableID: uint64(t.ID), Epoch: 0, Columns: t.Columns, PK: t.PKColumns,
		Dropped: map[core.ColID]int64{},
	}, nil
}

// loadSchemaAt reads one branch's schema version. epoch < 0 means the newest.
func (s *Store) loadSchemaAt(ctx context.Context, t *Table, branchID uuid.UUID, epoch int64) (
	*schemaeng.Version, error) {

	q := `SELECT epoch, columns, dropped FROM datagit_schema_version
	       WHERE table_id=$1 AND branch_id=$2`
	args := []any{t.ID, branchID}
	if epoch >= 0 {
		q += ` AND epoch <= $3 ORDER BY epoch DESC`
		args = append(args, epoch)
	} else {
		q += ` ORDER BY epoch DESC`
	}
	rows, err := s.pool.Direct().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var e int64
	var colsJSON, droppedJSON []byte
	if err := rows.Scan(&e, &colsJSON, &droppedJSON); err != nil {
		return nil, err
	}
	v := &schemaeng.Version{TableID: uint64(t.ID), Epoch: e, PK: t.PKColumns,
		Dropped: map[core.ColID]int64{}}
	if err := json.Unmarshal(colsJSON, &v.Columns); err != nil {
		return nil, fmt.Errorf("schema version %d is unreadable: %w", e, err)
	}
	dropped := map[string]int64{}
	if len(droppedJSON) > 0 {
		if err := json.Unmarshal(droppedJSON, &dropped); err != nil {
			return nil, fmt.Errorf("schema version %d has an unreadable dropped set: %w", e, err)
		}
	}
	for k, at := range dropped {
		// Parse at the width of core.ColID rather than through int: nextColID
		// derives the next column id from v.Dropped, so an entry lost to a
		// truncating conversion or a bad key would let it reissue an id an
		// earlier epoch already used (§10.5 rule 2).
		id, err := strconv.ParseUint(k, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("schema version %d has a bad dropped column id %q: %w", e, k, err)
		}
		v.Dropped[core.ColID(id)] = at
	}
	return v, nil
}

// SchemaChangeResult reports what a branch schema change did.
type SchemaChangeResult struct {
	Epoch   int64
	Changes []schemaeng.Change
	// Forked names the columns that got a NEW column id because the change was
	// narrowing (§10.5 rule 3). History is never coerced through a lossy cast, so
	// the old sidecar column keeps the old values and new writes go elsewhere.
	Forked []string
}

// AlterBranchSchema changes a branch's schema WITHOUT touching the live table.
//
// This is the branch half of §10.4. On a branch, a schema change is just a new
// schema version and a sidecar that can hold it; the live table is main's
// business and stays untouched until a plan is applied against it.
//
// Refused on the default branch: main's shape is what direct readers compiled
// against, and changing it is a migration, not a commit.
func (s *Store) AlterBranchSchema(ctx context.Context, repo *Repo, t *Table, branch string,
	want []adapter.Column, principal string) (*SchemaChangeResult, error) {

	if branch == DefaultBranch {
		return nil, fmt.Errorf(
			"cannot alter %q directly: main's shape is what direct readers compiled "+
				"against, so it changes through a migration plan, not a schema edit "+
				"(§10.4). Change the shape on a branch and propose it", DefaultBranch)
	}
	branchID, _, _, _, err := s.loadRef(ctx, s.pool.Direct(), repo, branch)
	if err != nil {
		return nil, err
	}
	from, err := s.LoadSchema(ctx, repo, t, branch)
	if err != nil {
		return nil, err
	}

	to := &schemaeng.Version{TableID: uint64(t.ID), Epoch: from.Epoch + 1,
		PK: from.PK, Dropped: map[core.ColID]int64{}}
	for k, v := range from.Dropped {
		to.Dropped[k] = v
	}

	next := nextColID(from)
	var forked []string
	for _, c := range want {
		old, existed := from.Column(c.ID)
		if existed && old.SQLType != c.SQLType {
			class, _ := schemaeng.ClassifyTypeChange(old.SQLType, c.SQLType)
			if class > adapter.Widening {
				// §10.5 rule 3. A narrowing or incompatible change allocates a NEW
				// column id: new writes go to the new sidecar column, old versions
				// stay in the old one, and projection reads whichever the version's
				// epoch names. Altering in place would coerce stored history through
				// a lossy cast, which is a silent rewrite of the past.
				to.Dropped[c.ID] = to.Epoch
				c.ID = next
				next++
				forked = append(forked, c.Name)
			}
		}
		to.Columns = append(to.Columns, c)
	}
	// Columns that existed and are gone from the request are dropped, not lost:
	// the sidecar column stays until retention has pruned every version using it
	// (§10.5 rule 2).
	present := map[core.ColID]bool{}
	for _, c := range to.Columns {
		present[c.ID] = true
	}
	for _, c := range from.Columns {
		if !present[c.ID] {
			to.Dropped[c.ID] = to.Epoch
		}
	}
	sort.Slice(to.Columns, func(i, j int) bool { return to.Columns[i].ID < to.Columns[j].ID })

	changes := schemaeng.Diff(from, to)
	if len(changes) == 0 {
		return &SchemaChangeResult{Epoch: from.Epoch}, nil
	}

	// The SIDECAR gets the new columns now, because branch writes need somewhere
	// to go. The LIVE table does not: that is what the migration plan is for.
	if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		spec := t.Spec()
		toSpec := *spec
		toSpec.Columns = to.Columns
		if err := s.ad.EvolveSidecar(ctx, tx, spec, &toSpec); err != nil {
			return fmt.Errorf("evolve sidecar for %s: %w", branch, err)
		}
		return s.writeSchemaVersion(ctx, tx, t, branchID, to, principal)
	}); err != nil {
		return nil, err
	}
	return &SchemaChangeResult{Epoch: to.Epoch, Changes: changes, Forked: forked}, nil
}

func nextColID(v *schemaeng.Version) core.ColID {
	var max core.ColID
	for _, c := range v.Columns {
		if c.ID > max {
			max = c.ID
		}
	}
	for id := range v.Dropped {
		if id > max {
			max = id
		}
	}
	return max + 1
}

func (s *Store) writeSchemaVersion(ctx context.Context, tx adapter.Tx, t *Table,
	branchID uuid.UUID, v *schemaeng.Version, principal string) error {

	cols, err := json.Marshal(v.Columns)
	if err != nil {
		return err
	}
	dropped := map[string]int64{}
	for id, at := range v.Dropped {
		// FormatUint, not Itoa(int(id)): core.ColID is uint32, and int is 32 bits
		// on a 32-bit build, so routing through it could write a negative key that
		// the ParseUint on the read side would then reject. This is the exact
		// mirror of that parse.
		dropped[strconv.FormatUint(uint64(id), 10)] = at
	}
	dj, _ := json.Marshal(dropped)

	hcols := make([]hash.SchemaColumn, 0, len(v.Columns))
	for _, c := range v.Columns {
		hcols = append(hcols, hash.SchemaColumn{
			ID: c.ID, Name: c.Name, Type: c.SQLType,
			Nullable: c.Nullable, PK: containsCol(v.PK, c.ID),
		})
	}
	d := hash.SchemaDigest(uint64(t.ID), hcols)
	// The mask width is recorded WITH the version, because changed_cols is over
	// column ids and only grows; comparing masks across epochs zero-extends the
	// shorter one (§10.5).
	width := int64(nextColID(v))

	return tx.Exec(ctx, s.ad.InsertOnConflict("datagit_schema_version",
		[]string{"table_id", "branch_id", "epoch", "columns", "dropped", "digest",
			"mask_width", "created_by"},
		"VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
		[]string{"table_id", "branch_id", "epoch"},
		[]string{"columns", "dropped", "digest", "mask_width"}),
		t.ID, branchID, v.Epoch, string(cols), string(dj), d[:], width, principal)
}

// MigrationPlanRecord is a plan awaiting a deliberate apply (§10.4).
type MigrationPlanRecord struct {
	ID          int64
	TableID     int64
	ProposalID  int64
	Ops         []adapter.MigrationOp
	TargetEpoch int64
	State       string
	CreatedBy   string
	// Confirm is set when the plan does something a direct reader could be
	// relying on, with one reason per operation. A plan that narrows or destroys
	// is never applied without someone saying so.
	Confirm []string
}

// SchemaMergeResult is what a schema merge produces.
type SchemaMergeResult struct {
	// Plan is nil when the branches agree on shape, which is the common case.
	Plan      *MigrationPlanRecord
	Conflicts []schemaeng.SchemaConflict
}

// MergeSchema reconciles two branches' schemas and records a migration plan
// (§10.3, §10.4).
//
// It does NOT apply anything. The plan is written to a table and left pending,
// because the live table it will eventually change is being read right now by
// applications that were compiled against its current shape.
func (s *Store) MergeSchema(ctx context.Context, repo *Repo, t *Table,
	from, into string, proposalID int64, principal string) (*SchemaMergeResult, error) {

	ours, err := s.LoadSchema(ctx, repo, t, into)
	if err != nil {
		return nil, err
	}
	theirs, err := s.LoadSchema(ctx, repo, t, from)
	if err != nil {
		return nil, err
	}
	// The base is the schema at the fork point. Using the branch's own earlier
	// epoch instead would call every inherited column an addition.
	base, err := s.LoadSchema(ctx, repo, t, DefaultBranch)
	if err != nil {
		return nil, err
	}

	out := schemaeng.Merge(base, ours, theirs)
	if len(out.Conflicts) > 0 {
		return &SchemaMergeResult{Conflicts: out.Conflicts}, nil
	}
	changes := schemaeng.Diff(ours, out.Result)
	if len(changes) == 0 {
		return &SchemaMergeResult{}, nil
	}

	plan := schemaeng.Plan(s.ad, uint64(t.ID), t.Physical, changes)
	needsConfirm, reasons := schemaeng.RequiresConfirmation(plan)
	rec := &MigrationPlanRecord{
		TableID: t.ID, ProposalID: proposalID, Ops: plan.Ops,
		TargetEpoch: ours.Epoch + 1, State: "pending", CreatedBy: principal,
	}
	if needsConfirm {
		rec.Confirm = reasons
	}

	opsJSON, err := json.Marshal(plan.Ops)
	if err != nil {
		return nil, err
	}
	if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		id, err := s.ad.InsertReturningID(ctx, tx,
			`INSERT INTO datagit_migration_plan
			   (repo_id, table_id, proposal_id, ops, target_epoch, state, created_by)
			 VALUES ($1,$2,$3,$4,$5,'pending',$6)`,
			repo.ID, t.ID, proposalID, string(opsJSON), rec.TargetEpoch, principal)
		if err != nil {
			return err
		}
		rec.ID = id
		// The merged schema is recorded against the TARGET branch now, so the plan
		// and the schema version cannot drift apart. The live table catches up when
		// the plan is applied.
		branchID, _, _, _, err := s.loadRef(ctx, tx, repo, into)
		if err != nil {
			return err
		}
		out.Result.Epoch = rec.TargetEpoch
		return s.writeSchemaVersion(ctx, tx, t, branchID, out.Result, principal)
	}); err != nil {
		return nil, err
	}
	return &SchemaMergeResult{Plan: rec}, nil
}

// LoadMigrationPlan reads a pending plan.
func (s *Store) LoadMigrationPlan(ctx context.Context, repo *Repo, id int64) (
	*MigrationPlanRecord, error) {

	rec := &MigrationPlanRecord{ID: id}
	var opsJSON []byte
	var proposal *int64
	if err := s.pool.Direct().QueryRow(ctx,
		`SELECT table_id, proposal_id, ops, target_epoch, state, created_by
		   FROM datagit_migration_plan WHERE id=$1 AND repo_id=$2`, id, repo.ID).
		Scan(&rec.TableID, &proposal, &opsJSON, &rec.TargetEpoch, &rec.State,
			&rec.CreatedBy); err != nil {
		return nil, fmt.Errorf("no migration plan %d: %w", id, err)
	}
	if proposal != nil {
		rec.ProposalID = *proposal
	}
	if err := json.Unmarshal(opsJSON, &rec.Ops); err != nil {
		return nil, fmt.Errorf("migration plan %d is unreadable: %w", id, err)
	}
	_, rec.Confirm = schemaeng.RequiresConfirmation(&adapter.MigrationPlan{Ops: rec.Ops})
	return rec, nil
}

// ListMigrationPlans returns a repository's plans, newest first.
func (s *Store) ListMigrationPlans(ctx context.Context, repo *Repo, states ...string) (
	[]MigrationPlanRecord, error) {

	q := `SELECT id, table_id, target_epoch, state, created_by
	        FROM datagit_migration_plan WHERE repo_id=$1`
	args := []any{repo.ID}
	if len(states) > 0 {
		cond, a := inList("state", states, len(args)+1)
		q += ` AND ` + cond
		args = append(args, a...)
	}
	q += ` ORDER BY id DESC`

	rows, err := s.pool.Direct().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MigrationPlanRecord
	for rows.Next() {
		var r MigrationPlanRecord
		if err := rows.Scan(&r.ID, &r.TableID, &r.TargetEpoch, &r.State, &r.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyMigrationPlan runs a pending plan against the live table (§10.4).
//
// The apply is a RESUMABLE JOURNALLED STATE MACHINE, not a transaction, and it
// runs that way on both engines. MySQL has no transactional DDL, so a
// multi-statement migration that fails halfway cannot be rolled back by the
// engine; PostgreSQL could use one transaction but does not, so failure
// behaviour is identical and only has to be tested once.
//
// A plan that narrows or destroys refuses to run without confirm, naming what it
// would do. The confirmation is the deliberate step the whole design is built
// around: this is the moment the live table changes shape under readers who
// never asked.
func (s *Store) ApplyMigrationPlan(ctx context.Context, repo *Repo, id int64,
	confirm bool, principal string) (*MigrationPlanRecord, error) {

	rec, err := s.LoadMigrationPlan(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	switch rec.State {
	case "applied":
		return nil, fmt.Errorf("migration plan %d is already applied", id)
	case "abandoned":
		return nil, fmt.Errorf("migration plan %d was abandoned", id)
	}
	if len(rec.Confirm) > 0 && !confirm {
		return nil, fmt.Errorf(
			"migration plan %d needs confirmation:\n  - %s\n"+
				"apply it with confirmation once readers can tolerate the change (§10.4)",
			id, joinLines(rec.Confirm))
	}

	if err := s.setPlanState(ctx, id, "applying", ""); err != nil {
		return nil, err
	}
	plan := &adapter.MigrationPlan{TableID: uint64(rec.TableID), Ops: rec.Ops}
	if err := s.ad.ApplyMigration(ctx, plan, s.Journal()); err != nil {
		// The state stays 'applying' rather than reverting to 'pending': the plan
		// is journalled per operation, so a retry RESUMES. Calling it pending again
		// would suggest nothing had happened, which is not true.
		_ = s.setPlanState(ctx, id, "failed", "")
		return nil, fmt.Errorf("apply migration plan %d (resume by re-applying): %w", id, err)
	}

	// The live table has the new shape now, so re-introspect and rewrite the
	// column catalogue: the ids are stable, but names and types have moved.
	if err := s.refreshColumns(ctx, repo, rec.TableID); err != nil {
		return nil, err
	}
	if err := s.setPlanState(ctx, id, "applied", principal); err != nil {
		return nil, err
	}
	rec.State = "applied"
	return rec, nil
}

// refreshColumns re-reads a live table's shape into the column catalogue after a
// migration changed it.
//
// Column IDS are preserved by name, because they are the identity every stored
// version and every changed_cols mask is written against (§10.5 rule 1).
// Reassigning them would orphan history.
func (s *Store) refreshColumns(ctx context.Context, repo *Repo, tableID int64) error {
	var physical string
	if err := s.pool.Direct().QueryRow(ctx,
		`SELECT physical_name FROM datagit_table WHERE id=$1`, tableID).Scan(&physical); err != nil {
		return err
	}
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		live, _, err := s.ad.Introspect(ctx, tx, physical)
		if err != nil {
			return err
		}
		known := map[string]core.ColID{}
		rows, err := tx.Query(ctx,
			`SELECT id, name FROM datagit_column WHERE table_id=$1`, tableID)
		if err != nil {
			return err
		}
		maxID := core.ColID(0)
		for rows.Next() {
			var id int32
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			known[name] = core.ColID(id)
			if core.ColID(id) > maxID {
				maxID = core.ColID(id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		seen := map[core.ColID]bool{}
		for i, c := range live {
			id, ok := known[c.Name]
			if !ok {
				maxID++
				id = maxID
			}
			seen[id] = true
			kind, _ := s.ad.KindFor(c.SQLType)
			if err := tx.Exec(ctx, s.ad.InsertOnConflict("datagit_column",
				[]string{"table_id", "id", "name", "sql_type", "kind", "nullable", "ordinal"},
				"VALUES ($1,$2,$3,$4,$5,$6,$7)",
				[]string{"table_id", "id"},
				[]string{"name", "sql_type", "kind", "nullable", "ordinal"}),
				tableID, int32(id), c.Name, c.SQLType, int16(kind), c.Nullable, i); err != nil {
				return err
			}
		}
		// A column that left the live table is marked dropped, never deleted: its
		// sidecar column still holds history (§10.5 rule 2).
		for name, id := range known {
			if seen[id] {
				continue
			}
			if err := tx.Exec(ctx,
				`UPDATE datagit_column SET dropped_at = $1
				  WHERE table_id = $2 AND id = $3 AND dropped_at IS NULL`,
				1, tableID, int32(id)); err != nil {
				return fmt.Errorf("mark %s dropped: %w", name, err)
			}
		}
		return nil
	})
}

func (s *Store) setPlanState(ctx context.Context, id int64, state, principal string) error {
	return s.pool.InTx(ctx, func(tx adapter.Tx) error {
		if state != "applied" {
			return tx.Exec(ctx,
				`UPDATE datagit_migration_plan SET state=$1 WHERE id=$2`, state, id)
		}
		at, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		return tx.Exec(ctx,
			`UPDATE datagit_migration_plan SET state='applied', applied_by=$1, applied_at=$2
			  WHERE id=$3`, principal, at, id)
	})
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n  - "
		}
		out += s
	}
	return out
}
