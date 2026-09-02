package catalog

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Glyph-Software/datagit/internal/adapter"
)

var (
	reTable = regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)\s*\(`)
	reWord  = regexp.MustCompile(`^\w+$`)
)

// tablesAndColumns parses a schema script well enough to compare two of them.
// It is not a SQL parser: it reads the leading identifier of each line inside a
// CREATE TABLE body and drops the ones that are constraint keywords.
func tablesAndColumns(script string) map[string][]string {
	out := map[string][]string{}
	var cur string
	depth := 0
	for _, line := range strings.Split(script, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		if m := reTable.FindStringSubmatch(t); m != nil {
			cur, depth = m[1], 1
			out[cur] = nil
			continue
		}
		if cur == "" {
			continue
		}
		depth += strings.Count(t, "(") - strings.Count(t, ")")
		if depth <= 0 {
			cur = ""
			continue
		}
		word := strings.Trim(strings.Fields(t)[0], "`\",")
		switch strings.ToUpper(word) {
		case "PRIMARY", "UNIQUE", "CONSTRAINT", "KEY", "FOREIGN", "CHECK", "INDEX", "REFERENCES":
			continue
		}
		if reWord.MatchString(word) {
			out[cur] = append(out[cur], word)
		}
	}
	return out
}

// TestControlSchemasDeclareTheSameShape is the drift guard.
//
// Two hand-written schemas can disagree, and the disagreement would surface as a
// query that works on one engine and fails on the other -- at runtime, on
// whichever engine the author was not using. Comparing them here turns that into
// a build failure. It cannot check types, which genuinely differ; it checks that
// every table and every column exists on both.
func TestControlSchemasDeclareTheSameShape(t *testing.T) {
	pg := tablesAndColumns(ControlSchema)
	my := tablesAndColumns(ControlSchemaMySQL)

	if len(pg) == 0 || len(my) == 0 {
		t.Fatalf("parsed %d PostgreSQL and %d MySQL tables; the parser is broken", len(pg), len(my))
	}
	for name, pgCols := range pg {
		myCols, ok := my[name]
		if !ok {
			t.Errorf("table %s exists on PostgreSQL but not MySQL", name)
			continue
		}
		set := map[string]bool{}
		for _, c := range myCols {
			set[c] = true
		}
		for _, c := range pgCols {
			if !set[c] {
				t.Errorf("%s.%s exists on PostgreSQL but not MySQL", name, c)
			}
		}
		rev := map[string]bool{}
		for _, c := range pgCols {
			rev[c] = true
		}
		for _, c := range myCols {
			if !rev[c] {
				t.Errorf("%s.%s exists on MySQL but not PostgreSQL", name, c)
			}
		}
	}
	for name := range my {
		if _, ok := pg[name]; !ok {
			t.Errorf("table %s exists on MySQL but not PostgreSQL", name)
		}
	}
}

func TestControlSchemaForSelectsByDialect(t *testing.T) {
	if ControlSchemaFor(adapter.MySQL) != ControlSchemaMySQL {
		t.Error("MySQL got the PostgreSQL schema")
	}
	if ControlSchemaFor(adapter.PostgreSQL) != ControlSchema {
		t.Error("PostgreSQL got the wrong schema")
	}
}
