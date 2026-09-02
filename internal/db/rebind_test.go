package db

import "testing"

func TestRebindReordersArguments(t *testing.T) {
	// $2 before $1 is the case a naive left-to-right replacement gets wrong.
	sql, order := Rebind(`SELECT * FROM t WHERE b = $2 AND a = $1`)
	if sql != `SELECT * FROM t WHERE b = ? AND a = ?` {
		t.Fatalf("sql = %q", sql)
	}
	got := Reorder([]any{"one", "two"}, order)
	if got[0] != "two" || got[1] != "one" {
		t.Errorf("args = %v, want [two one]: ? is positional, so the arguments "+
			"must be permuted to match", got)
	}
}

func TestRebindRepeatsArguments(t *testing.T) {
	// $1 twice is legal in PostgreSQL and impossible in MySQL without repeating
	// the value.
	sql, order := Rebind(`SELECT $1, $1, $2`)
	if sql != `SELECT ?, ?, ?` {
		t.Fatalf("sql = %q", sql)
	}
	got := Reorder([]any{"a", "b"}, order)
	if len(got) != 3 || got[0] != "a" || got[1] != "a" || got[2] != "b" {
		t.Errorf("args = %v, want [a a b]", got)
	}
}

func TestRebindLeavesStringLiteralsAlone(t *testing.T) {
	// A dollar inside a literal is data. Rewriting it would corrupt the value
	// and shift every later argument.
	sql, order := Rebind(`SELECT $1 WHERE label = 'costs $5' AND x = $2`)
	if sql != `SELECT ? WHERE label = 'costs $5' AND x = ?` {
		t.Errorf("sql = %q: a $ inside a literal is not a placeholder", sql)
	}
	if len(order) != 2 {
		t.Errorf("found %d placeholders, want 2", len(order))
	}
}

func TestRebindHandlesEscapedQuotes(t *testing.T) {
	sql, order := Rebind(`SELECT 'it''s $9 here', $1`)
	if sql != `SELECT 'it''s $9 here', ?` {
		t.Errorf("sql = %q", sql)
	}
	if len(order) != 1 || order[0] != 1 {
		t.Errorf("order = %v, want [1]", order)
	}
}

func TestQuoteToBacktick(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT "a" FROM "t"`, "SELECT `a` FROM `t`"},
		{`SELECT "a" FROM "t" WHERE x = 'say "hi"'`, "SELECT `a` FROM `t` WHERE x = 'say \"hi\"'"},
		{`SELECT "we""ird"`, "SELECT `we``ird`"},
		{`SELECT a FROM t`, `SELECT a FROM t`},
	}
	for _, c := range cases {
		if got := QuoteToBacktick(c.in); got != c.want {
			t.Errorf("QuoteToBacktick(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
