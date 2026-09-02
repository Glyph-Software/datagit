package schemaeng

import (
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/adapter/postgres"
	"github.com/Glyph-Software/datagit/internal/core"
)

func col(id int, name, typ string, nullable bool) adapter.Column {
	return adapter.Column{ID: core.ColID(id), Name: name, SQLType: typ, Nullable: nullable}
}

func ver(cols ...adapter.Column) *Version {
	return &Version{TableID: 1, Columns: cols, PK: []core.ColID{1}, Dropped: map[core.ColID]int64{}}
}

// TestClassificationDrivesTheRolloutWindow is the whole point of §10.4: whether
// a change can land silently or needs a deploy sequenced around it.
func TestClassificationDrivesTheRolloutWindow(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))

	cases := []struct {
		name  string
		to    *Version
		kind  ChangeKind
		class adapter.MigrationClass
	}{
		{"adding a nullable column is invisible to readers",
			ver(col(1, "sku", "text", false), col(2, "price", "integer", true),
				col(3, "margin", "numeric(5,2)", true)),
			AddColumn, adapter.Additive},

		{"int to bigint fits every existing value",
			ver(col(1, "sku", "text", false), col(2, "price", "bigint", true)),
			AlterColumnType, adapter.Widening},

		{"bigint to int can lose information",
			ver(col(1, "sku", "text", false), col(2, "price", "smallint", true)),
			AlterColumnType, adapter.Destructive},

		{"dropping NOT NULL only admits more values",
			ver(col(1, "sku", "text", true), col(2, "price", "integer", true)),
			DropNotNull, adapter.Widening},

		{"adding NOT NULL needs a pre-flight scan",
			ver(col(1, "sku", "text", false), col(2, "price", "integer", false)),
			SetNotNull, adapter.Narrowing},

		{"dropping a column breaks direct readers",
			ver(col(1, "sku", "text", false)),
			DropColumn, adapter.Destructive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := Diff(base, tc.to)
			if len(changes) != 1 {
				t.Fatalf("expected 1 change, got %d: %+v", len(changes), changes)
			}
			if changes[0].Kind != tc.kind {
				t.Errorf("kind is %s, want %s", changes[0].Kind, tc.kind)
			}
			if changes[0].Class != tc.class {
				t.Errorf("class is %d, want %d (%s)", changes[0].Class, tc.class, changes[0].Reason)
			}
		})
	}
}

// TestVarcharWideningVersusNarrowing: same base type, different modifier.
func TestVarcharWideningVersusNarrowing(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "name", "character varying(50)", true))

	wider := Diff(base, ver(col(1, "sku", "text", false), col(2, "name", "character varying(200)", true)))
	if wider[0].Class != adapter.Widening {
		t.Errorf("varchar(50) -> varchar(200) classified %d, want widening", wider[0].Class)
	}
	narrower := Diff(base, ver(col(1, "sku", "text", false), col(2, "name", "character varying(10)", true)))
	if narrower[0].Class != adapter.Narrowing {
		t.Errorf("varchar(50) -> varchar(10) classified %d, want narrowing", narrower[0].Class)
	}
}

// TestRenameIsNeverInferred: an inferred rename that guesses wrong maps one
// column's history onto another, which destroys data silently.
func TestRenameIsNeverInferred(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))
	// A different column id with a different name is a DROP plus an ADD, not a
	// rename, however similar the shapes look.
	to := ver(col(1, "sku", "text", false), col(3, "cost", "integer", true))
	changes := Diff(base, to)

	var adds, drops, renames int
	for _, c := range changes {
		switch c.Kind {
		case AddColumn:
			adds++
		case DropColumn:
			drops++
		case RenameColumn:
			renames++
		}
	}
	if renames != 0 {
		t.Error("a rename was inferred; that guess destroys data when it is wrong")
	}
	if adds != 1 || drops != 1 {
		t.Errorf("expected 1 add and 1 drop, got %d and %d", adds, drops)
	}
}

// TestRenameIsDetectedWhenDeclared: the same column id with a new name.
func TestRenameIsDetectedWhenDeclared(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))
	to := ver(col(1, "sku", "text", false), col(2, "unit_price", "integer", true))
	changes := Diff(base, to)
	if len(changes) != 1 || changes[0].Kind != RenameColumn {
		t.Fatalf("expected a rename, got %+v", changes)
	}
	if changes[0].Class != adapter.Destructive {
		t.Error("a rename must be destructive: existing readers query the old name")
	}
}

// TestSchemaMergeMatrix covers DESIGN.md §10.3.
func TestSchemaMergeMatrix(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))

	t.Run("both add the same column identically", func(t *testing.T) {
		c := col(3, "margin", "numeric(5,2)", true)
		out := Merge(base, ver(base.Columns[0], base.Columns[1], c),
			ver(base.Columns[0], base.Columns[1], c))
		if len(out.Conflicts) != 0 {
			t.Errorf("identical additions must merge clean: %v", out.Conflicts)
		}
	})

	t.Run("both add the same column differently", func(t *testing.T) {
		out := Merge(base,
			ver(base.Columns[0], base.Columns[1], col(3, "margin", "numeric(5,2)", true)),
			ver(base.Columns[0], base.Columns[1], col(3, "margin", "text", true)))
		if len(out.Conflicts) == 0 {
			t.Error("differing definitions of the same column must conflict")
		}
	})

	t.Run("one alters, one leaves alone", func(t *testing.T) {
		out := Merge(base,
			ver(base.Columns[0], col(2, "price", "bigint", true)),
			ver(base.Columns[0], base.Columns[1]))
		if len(out.Conflicts) != 0 {
			t.Errorf("a one-sided alteration must merge clean: %v", out.Conflicts)
		}
		c, _ := out.Result.Column(2)
		if c.SQLType != "bigint" {
			t.Errorf("the alteration was lost: type is %s, want bigint", c.SQLType)
		}
	})

	t.Run("both alter the same column differently", func(t *testing.T) {
		out := Merge(base,
			ver(base.Columns[0], col(2, "price", "bigint", true)),
			ver(base.Columns[0], col(2, "price", "numeric(12,2)", true)))
		if len(out.Conflicts) == 0 {
			t.Error("conflicting alterations must conflict")
		}
	})

	t.Run("one drops, the other writes to it", func(t *testing.T) {
		out := Merge(base,
			ver(base.Columns[0], col(2, "price", "bigint", true)), // ours altered it
			ver(base.Columns[0])) // theirs dropped it
		if len(out.Conflicts) == 0 {
			t.Error("drop on one side and alter on the other must conflict")
		}
	})

	t.Run("one drops, one leaves alone", func(t *testing.T) {
		out := Merge(base, ver(base.Columns[0], base.Columns[1]), ver(base.Columns[0]))
		if len(out.Conflicts) != 0 {
			t.Errorf("an uncontested drop must merge clean: %v", out.Conflicts)
		}
		if _, ok := out.Result.Dropped[2]; !ok {
			t.Error("the drop was not recorded")
		}
	})
}

// TestPlanOrdersSafeOperationsFirst: a partially applied plan must leave the
// table in a state existing readers can still use.
func TestPlanOrdersSafeOperationsFirst(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true),
		col(3, "legacy", "text", true))
	to := ver(col(1, "sku", "text", false), col(2, "price", "bigint", true),
		col(4, "margin", "numeric(5,2)", true))

	p := Plan(postgres.New(), 1, "products", Diff(base, to))
	if len(p.Ops) == 0 {
		t.Fatal("empty plan")
	}
	lastClass := adapter.MigrationClass(0)
	for _, op := range p.Ops {
		if op.Class < lastClass {
			t.Errorf("operation %s (class %d) runs after class %d: safe operations must come first",
				op.Kind, op.Class, lastClass)
		}
		lastClass = op.Class
		if !op.Idempotent {
			t.Errorf("operation %s is not idempotent; a resumed apply re-runs it", op.Kind)
		}
	}

	// A destructive drop is two-phase: deprecate, then drop.
	var deprecate, drop bool
	for _, op := range p.Ops {
		if op.Kind == "deprecate_column" {
			deprecate = true
		}
		if op.Kind == string(DropColumn) {
			if !deprecate {
				t.Error("a column was dropped without a deprecation phase: direct readers get no rollout window")
			}
			drop = true
		}
	}
	if !drop {
		t.Error("the plan never drops the removed column")
	}
}

// TestNarrowingGetsAPreflightScan: a plan that would fail must say so before it
// has changed anything.
func TestNarrowingGetsAPreflightScan(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))
	to := ver(col(1, "sku", "text", false), col(2, "price", "integer", false))
	p := Plan(postgres.New(), 1, "products", Diff(base, to))

	if len(p.Ops) < 2 || p.Ops[0].Kind != "preflight_not_null" {
		t.Fatalf("adding NOT NULL must be preceded by a pre-flight scan, got %+v", p.Ops)
	}
}

// TestRequiresConfirmationNamesTheRisk.
func TestRequiresConfirmationNamesTheRisk(t *testing.T) {
	base := ver(col(1, "sku", "text", false), col(2, "legacy", "text", true))
	safe := Plan(postgres.New(), 1, "products", Diff(base,
		ver(col(1, "sku", "text", false), col(2, "legacy", "text", true), col(3, "new", "text", true))))
	if need, _ := RequiresConfirmation(safe); need {
		t.Error("a purely additive plan must not require confirmation")
	}

	risky := Plan(postgres.New(), 1, "products", Diff(base, ver(col(1, "sku", "text", false))))
	need, reasons := RequiresConfirmation(risky)
	if !need || len(reasons) == 0 {
		t.Fatal("a destructive plan must require confirmation and say why")
	}
}

// TestProjectTreatsAbsentColumnsAsAbsent (§10.1). A column added after a commit
// must read as ABSENT at that commit, not NULL: NULL would assert the column
// existed and had no value, a claim about the past that was never true.
func TestProjectTreatsAbsentColumnsAsAbsent(t *testing.T) {
	old := ver(col(1, "sku", "text", false), col(2, "price", "integer", true))
	row := core.Row{1: core.Text("TENT-4P"), 2: core.Int(249), 3: core.MustNumeric("12.50")}

	got := old.Project(row)
	if _, present := got[3]; present {
		t.Error("a column that did not exist at this version was projected into the row")
	}
	if len(got) != 2 {
		t.Errorf("projection kept %d columns, want 2", len(got))
	}
}
