// Package schemaeng versions, diffs, and migrates table schemas
// (DESIGN.md §10).
//
// The governing constraint is §10.4: applications read the live tables directly,
// with compiled queries and ORM models that assume a shape. A data merge into the
// default branch applies immediately; a SCHEMA merge produces a migration plan
// that is applied deliberately. Instantly dropping a column would break every
// direct reader with no warning and no rollout window.
package schemaeng

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/core"
)

// Version is a table's schema at a commit (§10.1).
type Version struct {
	TableID uint64
	Epoch   int64
	Columns []adapter.Column
	PK      []core.ColID
	// Dropped records columns that existed earlier. §10.5 rule 2: a sidecar
	// column is never dropped while any retained version references its id, so
	// historical projection can still read it.
	Dropped map[core.ColID]int64
}

func (v *Version) Column(id core.ColID) (adapter.Column, bool) {
	for _, c := range v.Columns {
		if c.ID == id {
			return c, true
		}
	}
	return adapter.Column{}, false
}

// Project renders a row as it existed at this schema version (§10.1).
//
// A column added after a commit reads as ABSENT at that commit, not as NULL. The
// distinction matters: NULL asserts the column existed and had no value, which
// would be a claim about the past that was never true.
func (v *Version) Project(row core.Row) core.Row {
	out := core.Row{}
	for _, c := range v.Columns {
		if val, ok := row[c.ID]; ok {
			out[c.ID] = val
		}
	}
	return out
}

// ChangeKind is one structural schema operation.
type ChangeKind string

const (
	AddColumn       ChangeKind = "add_column"
	DropColumn      ChangeKind = "drop_column"
	AlterColumnType ChangeKind = "alter_column_type"
	SetNotNull      ChangeKind = "set_not_null"
	DropNotNull     ChangeKind = "drop_not_null"
	RenameColumn    ChangeKind = "rename_column"
)

// Change is one operation in a schema diff.
type Change struct {
	Kind   ChangeKind
	Col    core.ColID
	Name   string
	From   adapter.Column
	To     adapter.Column
	Class  adapter.MigrationClass
	Reason string
}

// Diff computes the structural difference between two schema versions (§10.2).
//
// Renames are detected ONLY when declared. An inferred rename that guesses wrong
// is a silent, data-destroying error — it would map one column's history onto
// another — so it is never inferred. A rename appears here only because the
// caller supplied it, and otherwise reads as a drop plus an add.
func Diff(from, to *Version) []Change {
	var out []Change
	have := map[core.ColID]adapter.Column{}
	for _, c := range from.Columns {
		have[c.ID] = c
	}
	seen := map[core.ColID]bool{}

	for _, c := range to.Columns {
		seen[c.ID] = true
		old, existed := have[c.ID]
		switch {
		case !existed:
			out = append(out, Change{
				Kind: AddColumn, Col: c.ID, Name: c.Name, To: c,
				Class: adapter.Additive,
				Reason: "a new nullable column is invisible to existing readers, " +
					"which simply ignore it",
			})
		case old.Name != c.Name && old.SQLType == c.SQLType:
			out = append(out, Change{
				Kind: RenameColumn, Col: c.ID, Name: c.Name, From: old, To: c,
				Class:  adapter.Destructive,
				Reason: "existing readers query the old name and will break",
			})
		case old.SQLType != c.SQLType:
			cls, why := classifyTypeChange(old.SQLType, c.SQLType)
			out = append(out, Change{
				Kind: AlterColumnType, Col: c.ID, Name: c.Name, From: old, To: c,
				Class: cls, Reason: why,
			})
		case old.Nullable && !c.Nullable:
			out = append(out, Change{
				Kind: SetNotNull, Col: c.ID, Name: c.Name, From: old, To: c,
				Class:  adapter.Narrowing,
				Reason: "requires a pre-flight scan proving no existing row is null",
			})
		case !old.Nullable && c.Nullable:
			out = append(out, Change{
				Kind: DropNotNull, Col: c.ID, Name: c.Name, From: old, To: c,
				Class:  adapter.Widening,
				Reason: "existing readers keep working; the column merely admits more values",
			})
		}
	}
	for _, c := range from.Columns {
		if !seen[c.ID] {
			out = append(out, Change{
				Kind: DropColumn, Col: c.ID, Name: c.Name, From: c,
				Class: adapter.Destructive,
				Reason: "every direct reader selecting this column breaks the moment it " +
					"is dropped, with no rollout window",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class // additive first, destructive last
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// widening lists the type transitions that cannot lose information.
var widening = map[string][]string{
	"smallint":          {"integer", "bigint", "numeric", "double precision"},
	"integer":           {"bigint", "numeric", "double precision"},
	"bigint":            {"numeric"},
	"real":              {"double precision"},
	"character varying": {"text"},
	"character":         {"character varying", "text"},
}

// ClassifyTypeChange reports how risky a type change is, and why.
//
// Exported because §10.5 rule 3 turns on it: anything past Widening forks to a
// new column id rather than altering in place.
func ClassifyTypeChange(from, to string) (adapter.MigrationClass, string) {
	return classifyTypeChange(from, to)
}

func classifyTypeChange(from, to string) (adapter.MigrationClass, string) {
	f, t := baseType(from), baseType(to)
	if f == t {
		// Same base type: compare the modifier, e.g. varchar(50) -> varchar(200).
		if fw, tw := width(from), width(to); fw > 0 && tw > 0 {
			if tw >= fw {
				return adapter.Widening, "the column admits everything it did before"
			}
			return adapter.Narrowing,
				"requires a pre-flight scan proving no existing value is too long"
		}
		return adapter.Widening, "no change in the value domain"
	}
	for _, w := range widening[f] {
		if w == t {
			return adapter.Widening, "every existing value fits the new type"
		}
	}
	return adapter.Destructive,
		fmt.Sprintf("%s to %s can lose information, and history cannot be coerced "+
			"through a lossy cast (§10.5 rule 3)", f, t)
}

// synonyms maps SQL spellings that name the SAME type to one canonical form.
//
// This is not an engine difference being papered over: these are standard
// synonyms, and both engines accept both spellings. It matters because the two
// engines REPORT different ones -- PostgreSQL introspects numeric(12,2) where
// MySQL reports decimal(12,2) for the identical declaration -- and treating them
// as different types would classify a widening as incompatible and fork the
// column to a new id for nothing (§10.5 rule 3).
//
// Only synonyms belong here. A type that merely converts cleanly is a WIDENING
// and belongs in the table above, where it stays visible as a change.
var synonyms = map[string]string{
	"decimal":                     "numeric",
	"dec":                         "numeric",
	"fixed":                       "numeric",
	"int":                         "integer",
	"int4":                        "integer",
	"int2":                        "smallint",
	"int8":                        "bigint",
	"bool":                        "boolean",
	"varchar":                     "character varying",
	"char":                        "character",
	"float4":                      "real",
	"float8":                      "double precision",
	"double":                      "double precision",
	"timestamptz":                 "timestamp with time zone",
	"datetime":                    "timestamp",
	"timestamp without time zone": "timestamp",
}

func baseType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// MySQL appends attributes to the reported type: "int unsigned", "bigint
	// unsigned zerofill". Unsigned is NOT a synonym -- an unsigned bigint holds
	// values a signed one cannot -- so it is kept as part of the name and a change
	// in signedness stays a change.
	if canonical, ok := synonyms[s]; ok {
		return canonical
	}
	return s
}

func width(s string) int {
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return 0
	}
	j := strings.IndexAny(s[i:], ",)")
	if j < 0 {
		return 0
	}
	n := 0
	for _, r := range s[i+1 : i+j] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// MergeOutcome is the result of merging two schema versions (§10.3).
type MergeOutcome struct {
	Result    *Version
	Conflicts []SchemaConflict
}

// SchemaConflict is a schema disagreement that cannot be resolved automatically.
type SchemaConflict struct {
	Col    core.ColID
	Name   string
	Reason string
}

// Merge reconciles two schema versions against their base (§10.3).
//
// Schema merges BEFORE data, because the data merge needs to know the shape it
// is producing.
func Merge(base, ours, theirs *Version) MergeOutcome {
	out := MergeOutcome{Result: &Version{
		TableID: base.TableID, PK: base.PK, Dropped: map[core.ColID]int64{}},
	}
	ids := map[core.ColID]bool{}
	for _, v := range []*Version{base, ours, theirs} {
		for _, c := range v.Columns {
			ids[c.ID] = true
		}
	}
	sorted := make([]core.ColID, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	for _, id := range sorted {
		b, inBase := base.Column(id)
		a, inOurs := ours.Column(id)
		t, inTheirs := theirs.Column(id)

		switch {
		case inOurs && inTheirs:
			if sameColumn(a, t) {
				out.Result.Columns = append(out.Result.Columns, a)
				continue
			}
			if inBase && sameColumn(a, b) {
				out.Result.Columns = append(out.Result.Columns, t) // only theirs altered
				continue
			}
			if inBase && sameColumn(t, b) {
				out.Result.Columns = append(out.Result.Columns, a) // only ours altered
				continue
			}
			kind := "altered"
			if !inBase {
				kind = "added"
			}
			out.Conflicts = append(out.Conflicts, SchemaConflict{
				Col: id, Name: a.Name,
				Reason: fmt.Sprintf("both branches %s this column differently: %s vs %s",
					kind, describe(a), describe(t)),
			})
		case inOurs && !inTheirs:
			if !inBase {
				out.Result.Columns = append(out.Result.Columns, a) // ours added it
				continue
			}
			// Theirs dropped it. If ours also altered it, that is a conflict.
			if !sameColumn(a, b) {
				out.Conflicts = append(out.Conflicts, SchemaConflict{
					Col: id, Name: a.Name,
					Reason: "one branch dropped this column while the other altered it",
				})
				continue
			}
			out.Result.Dropped[id] = 0 // dropped, with a warning at plan time
		case !inOurs && inTheirs:
			if !inBase {
				out.Result.Columns = append(out.Result.Columns, t)
				continue
			}
			if !sameColumn(t, b) {
				out.Conflicts = append(out.Conflicts, SchemaConflict{
					Col: id, Name: t.Name,
					Reason: "one branch dropped this column while the other altered it",
				})
				continue
			}
			out.Result.Dropped[id] = 0
		default:
			if inBase {
				out.Result.Dropped[id] = 0 // both dropped it
			}
		}
	}
	return out
}

func sameColumn(a, b adapter.Column) bool {
	return a.Name == b.Name && a.SQLType == b.SQLType && a.Nullable == b.Nullable
}

func describe(c adapter.Column) string {
	n := "NOT NULL"
	if c.Nullable {
		n = "NULL"
	}
	return fmt.Sprintf("%s %s %s", c.Name, c.SQLType, n)
}

// Plan turns a schema diff into a classified, ordered, resumable migration
// (§10.4).
//
// Ordering is deliberate: additive and widening operations first, so that a
// partially applied plan leaves the table in a state existing readers can still
// use. Destructive operations come last and are two-phase.
func Plan(ad adapter.Adapter, tableID uint64, physical string, changes []Change) *adapter.MigrationPlan {
	p := &adapter.MigrationPlan{TableID: tableID}
	ord := 0
	add := func(kind, sql string, class adapter.MigrationClass) {
		p.Ops = append(p.Ops, adapter.MigrationOp{
			Ordinal: ord, Kind: kind, SQL: sql, Class: class,
			// Every operation must be idempotent, because a crashed apply RESUMES
			// from the journal rather than restarting. S4 verified this converges
			// from every crash point on both engines.
			Idempotent: true,
		})
		ord++
	}
	// Column DDL is generated by the engine's own generator: it is one of the
	// least portable parts of SQL, and MySQL has to test the catalogue to make a
	// statement re-runnable at all (§4.3, §10.4).
	d := ad.DDL()

	for _, c := range changes {
		switch c.Kind {
		case AddColumn:
			add(string(c.Kind), d.AddColumn(physical, c.To.Name, c.To.SQLType), c.Class)
		case DropNotNull:
			add(string(c.Kind), d.DropNotNull(physical, c.To.Name, c.To.SQLType), c.Class)
		case AlterColumnType:
			add(string(c.Kind), d.AlterColumnType(physical, c.To.Name, c.To.SQLType), c.Class)
		case SetNotNull:
			// The pre-flight scan is its own journalled step, so a plan that would
			// fail says so before it has changed anything.
			add("preflight_not_null", d.PreflightNotNull(physical, c.To.Name), c.Class)
			add(string(c.Kind), d.SetNotNull(physical, c.To.Name, c.To.SQLType), c.Class)
		case RenameColumn:
			add(string(c.Kind), d.RenameColumn(physical, c.From.Name, c.To.Name), c.Class)
		case DropColumn:
			// Two-phase (§10.4). Phase one makes the column's pending removal
			// visible so a deploy can be sequenced around it; phase two drops it
			// after the window.
			add("deprecate_column", d.DeprecateColumn(physical, c.From.Name), c.Class)
			add(string(c.Kind), d.DropColumn(physical, c.From.Name), c.Class)
		}
	}
	return p
}

// RequiresConfirmation reports whether a plan changes anything a direct reader
// could be relying on (§10.4).
func RequiresConfirmation(p *adapter.MigrationPlan) (bool, []string) {
	var reasons []string
	for _, op := range p.Ops {
		switch op.Class {
		case adapter.Narrowing:
			reasons = append(reasons,
				fmt.Sprintf("%s narrows the column's value domain", op.Kind))
		case adapter.Destructive:
			reasons = append(reasons,
				fmt.Sprintf("%s breaks direct readers that use it, with no rollout window", op.Kind))
		}
	}
	return len(reasons) > 0, reasons
}
