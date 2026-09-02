package integration

import (
	"strings"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/crypto"
	"github.com/Glyph-Software/datagit/internal/store"
)

// piiFixture adds a table whose rows belong to data subjects, with
// crypto-shredding configured.
func setupPII(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)

	kek := make([]byte, crypto.KeyLen)
	for i := range kek {
		kek[i] = byte(i)
	}
	env, err := crypto.NewLocalEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	f.store = f.store.WithEnvelope(env)

	idType, tsType := "text", "timestamptz"
	if f.dialect == adapter.MySQL {
		idType, tsType = "varchar(64)", "datetime(6)"
	}
	must(t, f.pool.Direct().Exec(f.ctx, `
		CREATE TABLE contacts (
			id         `+idType+` PRIMARY KEY,
			customer   varchar(64),
			email      text,
			note       text,
			updated_at `+tsType+`
		)`))
	must(t, f.pool.Direct().Exec(f.ctx, `
		INSERT INTO contacts VALUES
			('C1', 'cust-1', 'ada@example.com',   'prefers email', '2026-03-02 00:00:00'),
			('C2', 'cust-2', 'grace@example.com', 'call after 5',  '2026-03-02 00:00:00')`))

	tbl, err := f.store.Track(f.ctx, f.repo, "contacts", adapter.ModeVersioned)
	if err != nil {
		t.Fatal(err)
	}
	f.table = tbl
	// email is personal data; customer identifies the subject it belongs to.
	if err := f.store.DesignatePII(f.ctx, f.repo, tbl, "email", "customer", principal); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) contactRow(t *testing.T, id, customer, email, note string) core.Row {
	t.Helper()
	c := f.table.Columns
	return core.Row{
		c[0].ID: core.Text(id), c[1].ID: core.Text(customer),
		c[2].ID: core.Text(email), c[3].ID: core.Text(note),
		c[4].ID: core.Null(),
	}
}

// TestPIIIsEncryptedInTheSidecarAndPlaintextLive is the §13.3 split that makes
// the whole mechanism possible: history is unreadable without a key, and direct
// readers need no key at all.
func TestPIIIsEncryptedInTheSidecarAndPlaintextLive(t *testing.T) {
	f := setupPII(t)
	pk := core.MakePK(core.Row{f.table.Columns[0].ID: core.Text("C1")}, f.table.PKColumns)

	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch,
		Author: principal, Message: "correct the address",
		Changes: []store.Change{{PK: pk, Op: core.OpUpdate,
			Row: f.contactRow(t, "C1", "cust-1", "ada.lovelace@example.com", "prefers email")}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The LIVE table is plaintext: a direct reader with no key still works, which
	// is the whole point of not putting DataGit on the read path.
	var live string
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT email FROM contacts WHERE id='C1'`).Scan(&live))
	if live != "ada.lovelace@example.com" {
		t.Errorf("the live table is not plaintext: %q", live)
	}

	// The SIDECAR is not. Reading the raw column must not yield the address.
	rows, err := f.pool.Direct().Query(f.ctx,
		`SELECT `+f.sidecarCol(2)+` FROM `+f.sidecarTable()+` WHERE `+f.sidecarCol(0)+` = 'C1'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		found = true
		if strings.Contains(string(raw), "@example.com") {
			t.Errorf("the sidecar holds the address in the clear: %q", raw)
		}
	}
	if !found {
		t.Fatal("no sidecar version was written")
	}

	// Through DataGit, with the key, history reads normally.
	hist, err := f.store.History(f.ctx, f.repo, f.table, store.DefaultBranch, pk)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) < 2 {
		t.Fatalf("expected at least 2 versions, got %d", len(hist))
	}
	if got := f.latestEmail(hist); got != "ada.lovelace@example.com" {
		t.Errorf("history read back %q, want the address", got)
	}
}

// TestErasureLeavesTheHashChainValid is the property that makes crypto-shredding
// worth its complexity: the data goes and the audit trail still proves itself.
func TestErasureLeavesTheHashChainValid(t *testing.T) {
	f := setupPII(t)
	pk1 := core.MakePK(core.Row{f.table.Columns[0].ID: core.Text("C1")}, f.table.PKColumns)

	if _, err := f.store.Commit(f.ctx, store.CommitRequest{
		Repo: f.repo, Table: f.table, Branch: store.DefaultBranch,
		Author: principal, Message: "update",
		Changes: []store.Change{{PK: pk1, Op: core.OpUpdate,
			Row: f.contactRow(t, "C1", "cust-1", "ada.lovelace@example.com", "prefers email")}},
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := f.store.EraseSubject(f.ctx, f.repo, f.table, "cust-1",
		"Article 17 request", "dpo@example.com")
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	if !rep.KeyDestroyed {
		t.Error("the key was not destroyed")
	}
	if rep.RowsErased != 1 {
		t.Errorf("erased %d current row(s), want 1", rep.RowsErased)
	}

	// The current row is gone from the live table.
	var n int
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM contacts WHERE customer='cust-1'`).Scan(&n))
	if n != 0 {
		t.Errorf("the subject's current rows survived erasure: %d remain", n)
	}
	// The other subject is untouched.
	must(t, f.pool.Direct().QueryRow(f.ctx,
		`SELECT count(*) FROM contacts WHERE customer='cust-2'`).Scan(&n))
	if n != 1 {
		t.Errorf("erasing cust-1 affected cust-2: %d rows remain, want 1", n)
	}

	// THE CHAIN STILL VERIFIES. No history row was modified, so it must.
	if err := f.store.VerifyIntegrity(f.ctx, f.repo, store.DefaultBranch); err != nil {
		t.Errorf("the hash chain broke after erasure: %v. Crypto-shredding exists "+
			"precisely so it does not: destroying a key touches no history row", err)
	}

	// History shows the marker, not a decryption error and not the address.
	hist, err := f.store.History(f.ctx, f.repo, f.table, store.DefaultBranch, pk1)
	if err != nil {
		t.Fatalf("history after erasure must still read: %v", err)
	}
	sawMarker := false
	for _, h := range hist {
		v := h.Row.Get(f.table.Columns[2].ID).Plain()
		if strings.Contains(v, "@example.com") {
			t.Errorf("an erased address is still readable in history: %q", v)
		}
		if v == store.ErasedMarker {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Error("history shows no erasure marker; an erasure is a fact the record should state")
	}

	erased, at, err := f.store.SubjectErased(f.ctx, f.repo, "cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if !erased || at == nil {
		t.Error("the erasure tombstone is missing; a deleted key row cannot be told " +
			"apart from a subject who never existed")
	}
}

// TestErasureIsRefusedWithoutDesignation: crypto-shredding protects what is
// designated and nothing else, and says so rather than pretending.
func TestErasureIsRefusedWithoutDesignation(t *testing.T) {
	f := setupPII(t)
	// products designates nothing.
	products, err := f.store.LoadTable(f.ctx, f.repo, "products")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.EraseSubject(f.ctx, f.repo, products, "cust-1", "test", "dpo@example.com")
	if err == nil {
		t.Fatal("erasure succeeded on a table with no designated PII")
	}
	if !strings.Contains(err.Error(), "designates no PII") {
		t.Errorf("the refusal does not explain the limit: %v", err)
	}
}

// TestPrimaryKeyCannotBeDesignatedPII: a row's key is its identity for all of
// history, and an unreadable identity orphans every version of it.
func TestPrimaryKeyCannotBeDesignatedPII(t *testing.T) {
	f := setupPII(t)
	err := f.store.DesignatePII(f.ctx, f.repo, f.table, "id", "customer", principal)
	if err == nil {
		t.Fatal("the primary key was accepted as a PII column")
	}
	if !strings.Contains(err.Error(), "primary key") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

func (f *fixture) sidecarTable() string { return `"datagit_v_contacts"` }

func (f *fixture) sidecarCol(i int) string {
	return `"c_` + itoa(int(f.table.Columns[i].ID)) + `"`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// latestEmail returns the email from the newest version that has one. History
// is ordered newest first.
func (f *fixture) latestEmail(hist []store.VersionRecord) string {
	for _, h := range hist {
		if v := h.Row.Get(f.table.Columns[2].ID); v.Kind != core.KindNull {
			return v.Plain()
		}
	}
	return ""
}
