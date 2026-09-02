package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/catalog"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/crypto"
	"github.com/Glyph-Software/datagit/internal/hash"
)

// Crypto-shredding: the erasure path that satisfies a right-to-erasure request
// without breaking the audit trail (§13.3).
//
// The tension it resolves: GDPR Article 17 says the data must go, and an audit
// trail says nothing may be rewritten. Both are satisfiable at once because
// "unreadable" is as good as "gone" for personal data, and unreadable can be
// achieved without touching a single stored row.
//
// Each data subject gets a key. PII values are encrypted under it IN THE SIDECAR
// ONLY. Erasure destroys the key, and every ciphertext for that subject -- across
// every commit, branch, backup, and replica -- becomes indistinguishable from
// random bytes at once. Because no history row changes, the hash chain still
// verifies.
//
// The live table stays PLAINTEXT. Encrypting it would put DataGit back on the
// read path for exactly the columns applications most need, which is the one
// thing the whole design exists to avoid.

// PIIColumn is a column designated as personal data.
type PIIColumn struct {
	ColumnID core.ColID
	Name     string
	// SubjectCol is the column of the SAME table holding the data subject's
	// identifier. Resolving a subject through a join would make erasure depend on
	// another table's current state, and erasure has to work from the row alone.
	SubjectCol  core.ColID
	SubjectName string
}

// DesignatePII marks a column as personal data belonging to a data subject
// (§13.3).
//
// The limit is stated rather than implied: this protects the columns named here
// and nothing else. Personal data that leaks into an undesignated free-text
// field is not covered, and no amount of key destruction will cover it.
func (s *Store) DesignatePII(ctx context.Context, repo *Repo, t *Table,
	column, subjectColumn, principal string) error {

	col, ok := t.ColumnByName(column)
	if !ok {
		return fmt.Errorf("%s has no column %q", t.Physical, column)
	}
	subj, ok := t.ColumnByName(subjectColumn)
	if !ok {
		return fmt.Errorf("%s has no column %q to resolve the data subject from",
			t.Physical, subjectColumn)
	}
	if containsCol(t.PKColumns, col.ID) {
		return fmt.Errorf(
			"%s.%s is part of the primary key and cannot be designated PII: the key "+
				"is a row's identity for all of history, and an unreadable identity "+
				"orphans every version of the row (§3.2)", t.Physical, column)
	}
	if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx, s.ad.InsertOnConflict("datagit_pii_column",
			[]string{"table_id", "column_id", "subject_col", "designated_by"},
			"VALUES ($1,$2,$3,$4)",
			[]string{"table_id", "column_id"},
			[]string{"subject_col", "designated_by"}),
			t.ID, int32(col.ID), int32(subj.ID), principal)
	}); err != nil {
		return err
	}
	// History that already exists is sealed now.
	//
	// Without this, designating a column protects only what is written AFTERWARDS
	// — and the backfill from track time, which is usually the largest body of
	// personal data in the sidecar, would stay in the clear through an erasure.
	// A designation that leaves the existing history readable is not a
	// designation.
	return s.sealExisting(ctx, repo, t, col.ID, subj.ID)
}

// sealExisting encrypts a designated column's already-stored sidecar values.
//
// It works version by version rather than as one UPDATE, because each ciphertext
// is bound to its row and column and needs a fresh nonce (§13.3).
func (s *Store) sealExisting(ctx context.Context, repo *Repo, t *Table,
	col, subjCol core.ColID) error {

	if s.envelope == nil {
		return fmt.Errorf(
			"no key envelope is configured, so designating %s would seal nothing "+
				"(§13.3)", t.Physical)
	}
	sc := quote(catalog.SidecarTable(t.Physical))
	colName := quote(catalog.SidecarColumn(uint32(col)))
	subjName := quote(catalog.SidecarColumn(uint32(subjCol)))

	pkNames := make([]string, 0, len(t.PKColumns))
	for _, id := range t.PKColumns {
		pkNames = append(pkNames, quote(catalog.SidecarColumn(uint32(id))))
	}

	type pending struct {
		branch  []byte
		seqFrom int64
		pkVals  []any
		subject string
		value   string
	}
	var todo []pending

	sel := fmt.Sprintf(
		`SELECT branch_id, seq_from, %s, %s, %s FROM %s WHERE %s IS NOT NULL`,
		strings.Join(pkNames, ", "), subjName, colName, sc, colName)
	rows, err := s.pool.Direct().Query(ctx, sel)
	if err != nil {
		return fmt.Errorf("scanning %s to seal existing history: %w", t.Physical, err)
	}
	for rows.Next() {
		p := pending{pkVals: make([]any, len(pkNames))}
		dest := []any{&p.branch, &p.seqFrom}
		for i := range p.pkVals {
			dest = append(dest, &p.pkVals[i])
		}
		var subj, val any
		dest = append(dest, &subj, &val)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return err
		}
		p.subject = asString(subj)
		p.value = asString(val)
		if p.subject != "" && !strings.HasPrefix(p.value, ciphertextPrefix) {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	keys := map[string][]byte{}
	for _, p := range todo {
		dek, ok := keys[p.subject]
		if !ok {
			var err error
			if dek, err = s.dekFor(ctx, repo, p.subject, true); err != nil {
				return err
			}
			keys[p.subject] = dek
		}
		vals := make([]core.Value, len(p.pkVals))
		row := core.Row{}
		for i, id := range t.PKColumns {
			c, _ := t.Column(id)
			v, err := fromDriver(p.pkVals[i], c.Kind)
			if err != nil {
				return err
			}
			vals[i], row[id] = v, v
		}
		pk := core.MakePK(row, t.PKColumns)
		ct, err := crypto.Encrypt(dek, []byte(p.value),
			crypto.AAD(uint64(t.ID), string(pk), uint32(col)))
		if err != nil {
			return err
		}
		sealed := ciphertextPrefix + base64.StdEncoding.EncodeToString(ct)

		where, args := sidecarPKWhere(t, vals, 3)
		if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
			return tx.Exec(ctx, fmt.Sprintf(
				`UPDATE %s SET %s = $1 WHERE branch_id = $2 AND %s`, sc, colName, where),
				append([]any{sealed, p.branch}, args...)...)
		}); err != nil {
			return fmt.Errorf("sealing existing history: %w", err)
		}
	}
	return nil
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// PIIColumns lists a table's designated columns.
func (s *Store) PIIColumns(ctx context.Context, t *Table) ([]PIIColumn, error) {
	rows, err := s.pool.Direct().Query(ctx,
		`SELECT column_id, subject_col FROM datagit_pii_column
		  WHERE table_id=$1 ORDER BY column_id`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PIIColumn
	for rows.Next() {
		var col, subj int32
		if err := rows.Scan(&col, &subj); err != nil {
			return nil, err
		}
		p := PIIColumn{ColumnID: core.ColID(col), SubjectCol: core.ColID(subj)}
		if c, ok := t.Column(p.ColumnID); ok {
			p.Name = c.Name
		}
		if c, ok := t.Column(p.SubjectCol); ok {
			p.SubjectName = c.Name
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SubjectFor resolves the data subject a row belongs to, or "" when the table
// designates no PII.
func (s *Store) SubjectFor(ctx context.Context, t *Table, row core.Row) (string, error) {
	cols, err := s.PIIColumns(ctx, t)
	if err != nil || len(cols) == 0 {
		return "", err
	}
	v := row.Get(cols[0].SubjectCol)
	if v.Kind == core.KindNull {
		return "", nil
	}
	return v.Plain(), nil
}

// dekFor returns a subject's key, creating one on first use.
//
// A destroyed key returns ErrErased rather than "not found": those mean very
// different things, and conflating them would let an erased subject silently
// acquire a fresh key and start accumulating readable history again.
func (s *Store) dekFor(ctx context.Context, repo *Repo, subject string, create bool) ([]byte, error) {
	if s.envelope == nil {
		return nil, fmt.Errorf(
			"no key envelope is configured: crypto-shredding needs one, because a lost " +
				"key is indistinguishable from an erased one and durability is the KMS's " +
				"problem, not DataGit's (§13.3)")
	}
	var wrapped []byte
	var erasedAt *time.Time
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT wrapped_dek, erased_at FROM datagit_dek WHERE repo_id=$1 AND subject=$2`,
		repo.ID, subject).Scan(&wrapped, &erasedAt)
	if err == nil {
		if erasedAt != nil || len(wrapped) == 0 {
			return nil, crypto.ErrErased
		}
		return s.envelope.Unwrap(wrapped)
	}
	if !create {
		return nil, crypto.ErrErased
	}

	dek, err := crypto.NewDEK()
	if err != nil {
		return nil, err
	}
	w, err := s.envelope.Wrap(dek)
	if err != nil {
		return nil, err
	}
	if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		return tx.Exec(ctx, s.ad.InsertOnConflict("datagit_dek",
			[]string{"repo_id", "subject", "wrapped_dek"},
			"VALUES ($1,$2,$3)", []string{"repo_id", "subject"}, nil),
			repo.ID, subject, w)
	}); err != nil {
		return nil, err
	}
	// Re-read: a concurrent writer may have won the insert, and using the key
	// that was actually stored is the only way both writers encrypt under one.
	return s.dekFor(ctx, repo, subject, false)
}

// sealer encrypts a commit's PII columns. It is built ONCE per commit rather
// than per row, so the key lookup does not repeat and the encryption can run
// inside the commit's own transaction.
//
// A nil sealer means the table designates no PII, which is the common case and
// costs nothing.
type sealer struct {
	cols    []PIIColumn
	tableID int64
	pk      []core.ColID
	// keys is per subject: one commit can touch rows belonging to many subjects.
	keys map[string][]byte
	get  func(subject string) ([]byte, error)
}

func (s *Store) newSealer(ctx context.Context, repo *Repo, t *Table) (*sealer, error) {
	if s.envelope == nil {
		return nil, nil
	}
	cols, err := s.PIIColumns(ctx, t)
	if err != nil || len(cols) == 0 {
		return nil, err
	}
	return &sealer{
		cols: cols, tableID: t.ID, pk: t.PKColumns, keys: map[string][]byte{},
		get: func(subject string) ([]byte, error) {
			return s.dekFor(ctx, repo, subject, true)
		},
	}, nil
}

// seal returns the row as it should be STORED IN THE SIDECAR.
//
// The live table and the commit hash both use the plaintext row, and that is not
// an oversight. AES-GCM uses a random nonce, so the same value encrypts
// differently every time; hashing the ciphertext would make two identical writes
// produce different commit ids and destroy the determinism the whole audit trail
// rests on (§12.1).
func (sl *sealer) seal(row core.Row) (core.Row, error) {
	if sl == nil || row == nil {
		return row, nil
	}
	subjVal := row.Get(sl.cols[0].SubjectCol)
	if subjVal.Kind == core.KindNull {
		return row, nil
	}
	subject := subjVal.Plain()
	dek, ok := sl.keys[subject]
	if !ok {
		var err error
		if dek, err = sl.get(subject); err != nil {
			return nil, err
		}
		sl.keys[subject] = dek
	}

	pk := core.MakePK(row, sl.pk)
	out := core.Row{}
	for id, v := range row {
		out[id] = v
	}
	for _, c := range sl.cols {
		v := row.Get(c.ColumnID)
		if v.Kind == core.KindNull {
			continue
		}
		ct, err := crypto.Encrypt(dek, []byte(v.Plain()),
			crypto.AAD(uint64(sl.tableID), string(pk), uint32(c.ColumnID)))
		if err != nil {
			return nil, err
		}
		// Base64, not raw bytes. The sidecar column MIRRORS the live column's type
		// (§5.2a), so a text column's sidecar is text and cannot hold arbitrary
		// bytes -- PostgreSQL rejects them outright as invalid UTF-8. The
		// alternative, altering the sidecar column to a binary type at designation
		// time, would make designating a column a schema migration. Base64 costs a
		// third more space on designated columns only, and keeps designation a
		// metadata change.
		out[c.ColumnID] = core.Text(ciphertextPrefix + base64.StdEncoding.EncodeToString(ct))
	}
	return out, nil
}

// ciphertextPrefix tags a sealed value.
//
// It exists so a value written BEFORE a column was designated can be told from
// one written after. Without it, designating a column would make every older
// plaintext value look like a failed decryption and read as erased -- which
// would be a lie about history.
const ciphertextPrefix = "datagit:enc:v1:"

func decodeCiphertext(v core.Value) ([]byte, bool) {
	if v.Kind != core.KindText || !strings.HasPrefix(v.Text, ciphertextPrefix) {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v.Text, ciphertextPrefix))
	if err != nil {
		return nil, false
	}
	return b, true
}

// ErasedMarker is what a historical read shows in place of a shredded value.
//
// A marker, not a decryption error: an erasure is a fact about the record, and
// the record should say so plainly rather than look corrupt.
const ErasedMarker = "<erased>"

// DecryptPII opens a row's designated columns, substituting the erased marker
// where the subject's key is gone.
func (s *Store) DecryptPII(ctx context.Context, repo *Repo, t *Table, row core.Row) (core.Row, error) {
	cols, err := s.PIIColumns(ctx, t)
	if err != nil || len(cols) == 0 {
		return row, err
	}
	subject, err := s.SubjectFor(ctx, t, row)
	if err != nil || subject == "" {
		return row, err
	}
	pk := core.MakePK(row, t.PKColumns)
	out := core.Row{}
	for id, v := range row {
		out[id] = v
	}
	dek, err := s.dekFor(ctx, repo, subject, false)
	if err != nil {
		for _, c := range cols {
			if row.Get(c.ColumnID).Kind != core.KindNull {
				out[c.ColumnID] = core.Text(ErasedMarker)
			}
		}
		return out, nil
	}
	for _, c := range cols {
		ct, ok := decodeCiphertext(row.Get(c.ColumnID))
		if !ok {
			// Not sealed: a value written before the column was designated. Left as
			// it is rather than reported as an error, because it IS the plaintext
			// and pretending otherwise would hide real history.
			continue
		}
		pt, err := crypto.Decrypt(dek, ct,
			crypto.AAD(uint64(t.ID), string(pk), uint32(c.ColumnID)))
		if err != nil {
			out[c.ColumnID] = core.Text(ErasedMarker)
			continue
		}
		out[c.ColumnID] = core.Text(string(pt))
	}
	return out, nil
}

// ErasureReport records what an erasure did.
type ErasureReport struct {
	Subject      string
	RowsErased   int
	KeyDestroyed bool
	Commit       hash.Digest
}

// EraseSubject satisfies a right-to-erasure request (§13.3).
//
// Two steps in one operation, and both are needed:
//
//  1. The subject's CURRENT rows on main are deleted by an ordinary commit. That
//     is what erasure means for current state, and it is the same kind of write
//     as any other -- it appears in the log, with an author and a message.
//  2. The subject's key is destroyed. Every historical ciphertext for them
//     becomes unreadable at once, across every commit, branch, backup, and
//     replica, without a single history row being modified.
//
// Because no history row changes, the hash chain still verifies and the audit
// trail remains provable. The erasure itself is a commit, so the record says who
// asked, who executed it, and when.
func (s *Store) EraseSubject(ctx context.Context, repo *Repo, t *Table,
	subject, reason, principal string) (*ErasureReport, error) {

	if principal == "" {
		return nil, fmt.Errorf("erasure requires an authenticated principal (§15.2)")
	}
	cols, err := s.PIIColumns(ctx, t)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf(
			"%s designates no PII columns, so there is no key to destroy: crypto-"+
				"shredding protects only what is designated (§13.3). Use `purge` for an "+
				"undesignated column, which is audited and irreversible", t.Physical)
	}
	rep := &ErasureReport{Subject: subject}

	// Step 1: the current rows go, as an ordinary commit.
	subjCol := cols[0].SubjectCol
	rows, err := s.Read(ctx, repo, t, ReadOptions{
		Branch: DefaultBranch,
		Filter: adapter.Compare{Col: subjCol, Op: adapter.Eq, Value: core.Text(subject)},
	})
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		changes := make([]Change, 0, len(rows))
		for _, r := range rows {
			changes = append(changes, Change{
				PK: core.MakePK(r, t.PKColumns), Op: core.OpDelete,
			})
		}
		res, err := s.Commit(ctx, CommitRequest{
			Repo: repo, Table: t, Branch: DefaultBranch, Changes: changes,
			Author:  principal,
			Message: fmt.Sprintf("Erase data subject %s: %s", subject, reason),
		})
		if err != nil {
			return nil, fmt.Errorf("erasing current rows: %w", err)
		}
		rep.RowsErased, rep.Commit = res.Changed, res.ID
	}

	// Step 2: the key goes. The ROW stays: a missing row cannot be told apart
	// from a subject who never existed, and the audit trail has to answer "was
	// this erased, and when".
	if err := s.pool.InTx(ctx, func(tx adapter.Tx) error {
		at, err := s.ad.Now(ctx, tx)
		if err != nil {
			return err
		}
		return tx.Exec(ctx,
			`UPDATE datagit_dek SET wrapped_dek = NULL, erased_at = $1, erased_by = $2
			  WHERE repo_id = $3 AND subject = $4`,
			at, principal, repo.ID, subject)
	}); err != nil {
		return nil, fmt.Errorf("destroying the key for %s: %w", subject, err)
	}
	rep.KeyDestroyed = true
	return rep, nil
}

// SubjectErased reports whether a subject's key has been destroyed.
func (s *Store) SubjectErased(ctx context.Context, repo *Repo, subject string) (bool, *time.Time, error) {
	var erasedAt *time.Time
	err := s.pool.Direct().QueryRow(ctx,
		`SELECT erased_at FROM datagit_dek WHERE repo_id=$1 AND subject=$2`,
		repo.ID, subject).Scan(&erasedAt)
	if err != nil {
		return false, nil, nil // no key was ever issued
	}
	return erasedAt != nil, erasedAt, nil
}

var _ = catalog.SidecarTable
