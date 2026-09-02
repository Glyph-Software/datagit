// Command datagit is the reference client (PLAN.md M1.13, §16.3).
//
// It talks to the store directly in M1. When the gRPC surface lands (M1.10) it
// becomes a client of that instead, and remains the reference implementation
// that must exercise every endpoint a UI would need.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Glyph-Software/datagit/internal/adapter"
	"github.com/Glyph-Software/datagit/internal/connect"
	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/hash"
	"github.com/Glyph-Software/datagit/internal/store"
)

const usage = `datagit — Git-style version control for the data in your database

Usage:
  datagit <command> [flags]

Repository
  repo init <name>          Create the control tables and a repository
  status                    Show the repository, its tables, and branch heads

Tables
  track <table>             Bring a live table under version control
  untrack <table>           Remove a table; the live table is left untouched
  export <table>            Write the full history as newline-delimited JSON

Reading
  read <table>              Resolve a table (--at, --as-of, --where, --limit)
  log                       Show the commit history
  history <table>           Show one row's version chain (--pk)
  blame <table>             Attribute each cell of one row (--pk)
  diff <table>              Show what changed between two commits (--from-seq, --to-seq)

Writing
  commit <table>            Apply a change set (--set, --delete, -m)
  revert <commit>           A new commit that undoes a prior one; erases nothing

Branching
  branch [list]             List branches and tags
  branch create <name>      Fork a branch (--from); O(1), copies no data
  branch delete <name>      Delete a branch
  branch protect <name>     Require approvals to merge (--approvals)
  merge <from>              Merge a branch (--table, --into)
  update-from-parent        Absorb the parent's newer commits (--table)
  materialize <branch>      Copy a branch into a real schema (--into)

Schema
  schema show               Show a branch's schema (--table, --branch)
  schema add-column         Add a column ON A BRANCH (--table, --branch,
                            --column, --type); the live table is untouched
  schema drop-column        Drop a column on a branch (--table, --branch, --column)
  migration [list]          Migration plans awaiting apply
  migration show <id>       What a plan would do to the live table
  migration apply <id>      Apply it (--confirm for narrowing or destructive)

Review
  proposal [list]           List change proposals
  proposal create           Open one (--from, --into, --title)
  proposal approve|comment|reject   Review one (--id, -m)
  proposal conflicts        Show a conflicted proposal (--id, --table)
  proposal merge            Merge an approved proposal (--id, --table)

Verification and operations
  verify                    Check drift and hash-chain integrity
  gc                        Reclaim unreachable versions and expired sessions
  prune                     Apply a retention policy (--table, --keep-commits)
  purge <table>             Physically erase a row (--pk, --reason); audited

Compliance
  pii list                  Show designated PII columns (--table)
  pii designate             Designate a column (--table, --column, --subject-column)
  erase <subject>           Right-to-erasure: delete current rows and destroy
                            the subject's key (--table, --reason). History stays
                            hash-verifiable; erased values read as <erased>.

Global flags:
  --dsn      PostgreSQL connection string (or $DATAGIT_DSN)
  --repo     Repository name (default "default")
  --branch   Branch name (default "main")
  --author   Authenticated principal (or $DATAGIT_AUTHOR)
`

type globals struct {
	dsn    string
	repo   string
	branch string
	author string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	g := &globals{}
	fs.StringVar(&g.dsn, "dsn", env("DATAGIT_DSN", "postgres://datagit:datagit@localhost:55417/datagit"),
		"database DSN; the engine is detected from it (PostgreSQL or MySQL)")
	fs.StringVar(&g.repo, "repo", env("DATAGIT_REPO", "default"), "repository name")
	fs.StringVar(&g.branch, "branch", env("DATAGIT_BRANCH", store.DefaultBranch), "branch name")
	fs.StringVar(&g.author, "author", env("DATAGIT_AUTHOR", ""), "authenticated principal")

	// Subcommand flags are all registered here rather than per handler, so that
	// `datagit <cmd> --help` lists everything and handlers can look up whatever
	// they need without each one re-declaring the shared ones.
	fs.String("mode", "versioned", "track: audit | versioned")
	fs.String("pk", "", "primary key, as column=value[,column=value]")
	fs.String("set", "", "commit: column=value; repeat with ';'")
	fs.Bool("delete", false, "commit: delete the row")
	fs.String("m", "", "commit message")
	fs.String("ref", "", "external reference, such as a ticket id")
	fs.String("at", "", "read: resolve at this commit id")
	fs.String("as-of", "", "read: resolve as of this RFC3339 timestamp")
	fs.String("column", "", "blame: restrict to one column; schema: the column to add or drop")
	fs.String("table", "", "revert: the table to revert within")
	fs.Bool("force", false, "revert: proceed even if later changes would be discarded")
	fs.Int("limit", 0, "maximum rows or commits")
	fs.Int("from-seq", 0, "diff: starting sequence")
	fs.Int("to-seq", -1, "diff: ending sequence (default: head)")
	fs.String("from", "", "branch/proposal: source branch")
	fs.String("into", "", "merge/proposal/materialize: target branch or schema")
	fs.String("type", "", "schema add-column: SQL type")
	fs.String("subject-column", "", "pii designate: the column identifying the data subject")
	fs.String("kek-file", env("DATAGIT_KEK_FILE", ""),
		"file holding the 32-byte key-encryption key for crypto-shredding")
	fs.Bool("confirm", false, "migration apply: confirm a narrowing or destructive plan")
	fs.String("title", "", "proposal: title")
	fs.String("reason", "", "purge: stated reason (required)")
	fs.Int("id", 0, "proposal id")
	fs.Int("approvals", 1, "branch protect: approvals required")
	fs.Int("keep-days", 0, "prune: keep history for N days")
	fs.Int("keep-commits", 0, "prune: keep the last N commits")

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "repo":
		return cmdRepo(fs, g, rest)
	case "status":
		return withStore(fs, g, rest, cmdStatus)
	case "track":
		return withStore(fs, g, rest, cmdTrack)
	case "untrack":
		return withStore(fs, g, rest, cmdUntrack)
	case "export":
		return withStore(fs, g, rest, cmdExport)
	case "read":
		return withStore(fs, g, rest, cmdRead)
	case "log":
		return withStore(fs, g, rest, cmdLog)
	case "history":
		return withStore(fs, g, rest, cmdHistory)
	case "blame":
		return withStore(fs, g, rest, cmdBlame)
	case "diff":
		return withStore(fs, g, rest, cmdDiff)
	case "commit":
		return withStore(fs, g, rest, cmdCommit)
	case "revert":
		return withStore(fs, g, rest, cmdRevert)
	case "verify":
		return withStore(fs, g, rest, cmdVerify)
	case "branch":
		return withStore(fs, g, rest, cmdBranch)
	case "merge":
		return withStore(fs, g, rest, cmdMerge)
	case "update-from-parent":
		return withStore(fs, g, rest, cmdUpdateFrom)
	case "proposal":
		return withStore(fs, g, rest, cmdProposal)
	case "materialize":
		return withStore(fs, g, rest, cmdMaterialize)
	case "schema":
		return withStore(fs, g, rest, cmdSchema)
	case "migration":
		return withStore(fs, g, rest, cmdMigration)
	case "gc":
		return withStore(fs, g, rest, cmdGC)
	case "prune":
		return withStore(fs, g, rest, cmdPrune)
	case "purge":
		return withStore(fs, g, rest, cmdPurge)
	case "pii":
		return withStore(fs, g, rest, cmdPII)
	case "erase":
		return withStore(fs, g, rest, cmdErase)
	}
	return fmt.Errorf("unknown command %q (try: datagit help)", cmd)
}

type env2 struct {
	ctx  context.Context
	st   *store.Store
	repo *store.Repo
	g    *globals
	args []string
	fs   *flag.FlagSet
}

func withStore(fs *flag.FlagSet, g *globals, args []string, fn func(*env2) error) error {
	e := &env2{g: g, fs: fs}
	flags, positional := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	e.args = positional

	ctx := context.Background()
	pool, ad, err := connect.Open(ctx, g.dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool, ad)
	if err := st.CheckControlSchema(ctx); err != nil {
		return err
	}
	repo, err := st.LookupRepo(ctx, g.repo)
	if err != nil {
		return err
	}
	e.ctx, e.st, e.repo = ctx, st, repo
	return fn(e)
}

func cmdRepo(fs *flag.FlagSet, g *globals, args []string) error {
	flags, positional := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] != "init" {
		return fmt.Errorf("usage: datagit repo init <name>")
	}
	name := positional[1]
	if g.author == "" {
		return fmt.Errorf("--author is required: commits carry a verified principal (§15.2)")
	}
	ctx := context.Background()
	pool, ad, err := connect.Open(ctx, g.dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool, ad)
	if err := st.InitControlSchema(ctx); err != nil {
		return err
	}
	repo, err := st.CreateRepo(ctx, name, g.author)
	if err != nil {
		return err
	}
	fmt.Printf("initialized repository %q (%s)\n", repo.Name, repo.ID)
	return nil
}

func cmdStatus(e *env2) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "repository\t%s\n", e.repo.Name)
	tables, err := e.st.ListTables(e.ctx, e.repo)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nTABLE\tMODE\tSTATE\tCOLUMNS")
	for _, t := range tables {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", t.Physical, t.Mode, t.State, len(t.Columns))
	}
	return w.Flush()
}

func cmdTrack(e *env2) error {
	mode := strFlag(e.fs, "mode")
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit track <table> [--mode versioned|audit]")
	}
	t, err := e.st.Track(e.ctx, e.repo, e.args[0], adapter.Mode(mode))
	if err != nil {
		return err
	}
	fmt.Printf("tracking %s in %s mode (%d columns, primary key %d column(s))\n",
		t.Physical, t.Mode, len(t.Columns), len(t.PKColumns))
	return nil
}

func cmdUntrack(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit untrack <table>")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, e.args[0])
	if err != nil {
		return err
	}
	if err := e.st.Untrack(e.ctx, e.repo, t); err != nil {
		return err
	}
	fmt.Printf("untracked %s — the live table is unchanged\n", t.Physical)
	return nil
}

func cmdExport(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit export <table>")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, e.args[0])
	if err != nil {
		return err
	}
	return e.st.Export(e.ctx, e.repo, t, e.g.branch, os.Stdout)
}

func cmdRead(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit read <table> [--at <commit>] [--as-of <ts>] [--limit n]")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, e.args[0])
	if err != nil {
		return err
	}
	opt := store.ReadOptions{Branch: e.g.branch, Limit: intFlag(e.fs, "limit")}
	if v := strFlag(e.fs, "at"); v != "" {
		d, err := parseDigest(v)
		if err != nil {
			return err
		}
		opt.At = &d
	}
	if v := strFlag(e.fs, "as-of"); v != "" {
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fmt.Errorf("--as-of must be RFC3339: %w", err)
		}
		opt.AsOf = &ts
	}
	rows, err := e.st.Read(e.ctx, e.repo, t, opt)
	if err != nil {
		return err
	}
	return printRows(t, rows)
}

func cmdLog(e *env2) error {
	commits, err := e.st.Log(e.ctx, e.repo, e.g.branch, intFlagOr(e.fs, "limit", 20))
	if err != nil {
		return err
	}
	for _, c := range commits {
		fmt.Printf("%s  %-22s  %s\n", c.ID.Short(), c.Author, c.CommittedAt.Format(time.RFC3339))
		fmt.Printf("    %s", c.Message)
		if c.ExternalRef != "" {
			fmt.Printf("  [%s]", c.ExternalRef)
		}
		fmt.Println()
	}
	return nil
}

func cmdHistory(e *env2) error {
	t, pk, err := tableAndPK(e)
	if err != nil {
		return err
	}
	recs, err := e.st.History(e.ctx, e.repo, t, e.g.branch, pk)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMIT\tOP\tSEQ\tAUTHOR\tWHEN\tMESSAGE")
	for _, r := range recs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", r.CommitID.Short(), r.Op, r.SeqFrom,
			r.Author, r.At.Format("2006-01-02"), r.Message)
	}
	return w.Flush()
}

func cmdBlame(e *env2) error {
	t, pk, err := tableAndPK(e)
	if err != nil {
		return err
	}
	var cols []core.ColID
	if v := strFlag(e.fs, "column"); v != "" {
		for _, c := range t.Columns {
			if c.Name == v {
				cols = append(cols, c.ID)
			}
		}
		if len(cols) == 0 {
			return fmt.Errorf("no column %q in %s", v, t.Physical)
		}
	}
	blame, err := e.st.Blame(e.ctx, e.repo, t, e.g.branch, pk, cols)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tVALUE\tCOMMIT\tAUTHOR\tWHEN\tMESSAGE")
	for _, b := range blame {
		c, _ := t.Column(b.Col)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, b.Value, b.CommitID.Short(),
			b.Author, b.At.Format("2006-01-02"), b.Message)
	}
	return w.Flush()
}

func cmdDiff(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit diff <table> --from-seq <n> [--to-seq <n>]")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, e.args[0])
	if err != nil {
		return err
	}
	entries, err := e.st.Diff(e.ctx, e.repo, t, e.g.branch,
		int64(intFlag(e.fs, "from-seq")), int64(intFlagOr(e.fs, "to-seq", -1)))
	if err != nil {
		return err
	}
	adds, dels, mods := 0, 0, 0
	for _, d := range entries {
		switch d.Op {
		case core.OpInsert:
			adds++
		case core.OpDelete:
			dels++
		default:
			mods++
		}
	}
	fmt.Printf("~ %s  %d modified, %d added, %d removed\n", t.Physical, mods, adds, dels)
	for _, d := range entries {
		key := core.PKString(pickRow(d), t.PKColumns)
		switch d.Op {
		case core.OpInsert:
			fmt.Printf("  + %s[%s]\n", t.Physical, key)
		case core.OpDelete:
			fmt.Printf("  - %s[%s]\n", t.Physical, key)
		default:
			fmt.Printf("  ~ %s[%s]\n", t.Physical, key)
			for _, cid := range d.Changed.Cols() {
				c, ok := t.Column(cid)
				if !ok {
					continue
				}
				fmt.Printf("      %-12s %s  →  %s\n", c.Name, d.Before.Get(cid), d.After.Get(cid))
			}
		}
	}
	return nil
}

func pickRow(d store.DiffEntry) core.Row {
	if d.After != nil {
		return d.After
	}
	return d.Before
}

func cmdCommit(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit commit <table> --pk k=v [--set c=v ...] [--delete] -m <message>")
	}
	if e.g.author == "" {
		return fmt.Errorf("--author is required: commits carry a verified principal (§15.2)")
	}
	t, pk, err := tableAndPK(e)
	if err != nil {
		return err
	}
	msg := strFlag(e.fs, "m")
	if msg == "" {
		return fmt.Errorf("-m <message> is required")
	}

	var ch store.Change
	if boolFlag(e.fs, "delete") {
		ch = store.Change{PK: pk, Op: core.OpDelete}
	} else {
		rows, err := e.st.Read(e.ctx, e.repo, t, store.ReadOptions{Branch: e.g.branch})
		if err != nil {
			return err
		}
		var cur core.Row
		for _, r := range rows {
			if core.MakePK(r, t.PKColumns) == pk {
				cur = r
			}
		}
		if cur == nil {
			return fmt.Errorf("no row with that primary key (insert is not yet wired into the CLI)")
		}
		next := cur.Clone()
		for _, kv := range strSliceFlag(e.fs, "set") {
			name, val, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--set expects column=value, got %q", kv)
			}
			col, found := columnByName(t, name)
			if !found {
				return fmt.Errorf("no column %q in %s", name, t.Physical)
			}
			v, err := parseValue(val, col.Kind)
			if err != nil {
				return fmt.Errorf("--set %s: %w", name, err)
			}
			next[col.ID] = v
		}
		ch = store.Change{PK: pk, Op: core.OpUpdate, Row: next}
	}

	res, err := e.st.Commit(e.ctx, store.CommitRequest{
		Repo: e.repo, Table: t, Branch: e.g.branch, Changes: []store.Change{ch},
		Author: e.g.author, Message: msg, ExternalRef: strFlag(e.fs, "ref"),
	})
	if err != nil {
		return err
	}
	if res.Changed == 0 {
		fmt.Println("nothing changed; no commit was created")
		return nil
	}
	fmt.Printf("%s  %d row(s) changed\n", res.ID.Short(), res.Changed)
	return nil
}

func cmdRevert(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit revert <commit> --table <table> [--force]")
	}
	if e.g.author == "" {
		return fmt.Errorf("--author is required")
	}
	d, err := parseDigest(e.args[0])
	if err != nil {
		return err
	}
	name := strFlag(e.fs, "table")
	if name == "" {
		return fmt.Errorf("--table is required")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, name)
	if err != nil {
		return err
	}
	res, err := e.st.Revert(e.ctx, e.repo, t, e.g.branch, d, e.g.author,
		strFlag(e.fs, "m"), boolFlag(e.fs, "force"))
	if err != nil {
		return err
	}
	fmt.Printf("%s  reverted %s (%d row(s))\n", res.ID.Short(), d.Short(), res.Changed)
	return nil
}

func cmdVerify(e *env2) error {
	tables, err := e.st.ListTables(e.ctx, e.repo)
	if err != nil {
		return err
	}
	failed := false
	fmt.Println("integrity: recomputing the commit hash chain")
	if err := e.st.VerifyIntegrity(e.ctx, e.repo, e.g.branch); err != nil {
		fmt.Printf("  FAIL  %v\n", err)
		failed = true
	} else {
		fmt.Println("  ok")
	}
	fmt.Println("drift: comparing live tables against the resolved branch")
	for _, t := range tables {
		rep, err := e.st.VerifyDrift(e.ctx, e.repo, t)
		if err != nil {
			return err
		}
		if rep.LiveOnly+rep.VersionOnly+rep.Mismatched == 0 {
			fmt.Printf("  ok    %s\n", t.Physical)
			continue
		}
		fmt.Printf("  FAIL  %s: %d only in the live table, %d only in history, %d mismatched\n",
			t.Physical, rep.LiveOnly, rep.VersionOnly, rep.Mismatched)
		failed = true
	}
	if failed {
		return fmt.Errorf("verification failed")
	}
	return nil
}

// --- helpers ---

func tableAndPK(e *env2) (*store.Table, core.PK, error) {
	if len(e.args) < 1 {
		return nil, "", fmt.Errorf("a table name is required")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, e.args[0])
	if err != nil {
		return nil, "", err
	}
	spec := strFlag(e.fs, "pk")
	if spec == "" {
		return nil, "", fmt.Errorf("--pk column=value is required")
	}
	row := core.Row{}
	for _, part := range strings.Split(spec, ",") {
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, "", fmt.Errorf("--pk expects column=value, got %q", part)
		}
		col, found := columnByName(t, name)
		if !found {
			return nil, "", fmt.Errorf("no column %q in %s", name, t.Physical)
		}
		v, err := parseValue(val, col.Kind)
		if err != nil {
			return nil, "", err
		}
		row[col.ID] = v
	}
	return t, core.MakePK(row, t.PKColumns), nil
}

func columnByName(t *store.Table, name string) (adapter.Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return adapter.Column{}, false
}

func parseValue(s string, kind core.Kind) (core.Value, error) {
	switch kind {
	case core.KindText:
		return core.Text(s), nil
	case core.KindNumeric:
		return core.Numeric(s)
	case core.KindInt:
		var n int64
		if _, err := fmt.Sscan(s, &n); err != nil {
			return core.Value{}, fmt.Errorf("%q is not an integer", s)
		}
		return core.Int(n), nil
	case core.KindFloat:
		var f float64
		if _, err := fmt.Sscan(s, &f); err != nil {
			return core.Value{}, fmt.Errorf("%q is not a number", s)
		}
		return core.Float(f), nil
	case core.KindBool:
		return core.Bool_(s == "true" || s == "t" || s == "1"), nil
	case core.KindTime:
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return core.Value{}, fmt.Errorf("%q is not an RFC3339 timestamp", s)
		}
		return core.Time(ts), nil
	}
	return core.Text(s), nil
}

func printRows(t *store.Table, rows []core.Row) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	names := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		names = append(names, strings.ToUpper(c.Name))
	}
	fmt.Fprintln(w, strings.Join(names, "\t"))
	for _, r := range rows {
		vals := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			vals = append(vals, trimQuotes(r.Get(c.ID).String()))
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d row(s)\n", len(rows))
	return nil
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func parseDigest(s string) (hash.Digest, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return hash.Digest{}, fmt.Errorf("%q is not a hex commit id", s)
	}
	var d hash.Digest
	if len(b) != len(d) {
		return hash.Digest{}, fmt.Errorf("commit id must be %d hex bytes, got %d", len(d), len(b))
	}
	copy(d[:], b)
	return d, nil
}

// splitArgs separates flags from positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `datagit track products --mode audit` would silently ignore --mode. Users
// reasonably write the table name first, so the two are separated here rather
// than imposing an order.
func splitArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		// A non-boolean flag consumes the next argument as its value.
		name := strings.TrimLeft(a, "-")
		if f := fs.Lookup(name); f != nil {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return flags, positional
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Flag accessors that tolerate a flag not being registered by this subcommand.
func strFlag(fs *flag.FlagSet, name string) string {
	if f := fs.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

func boolFlag(fs *flag.FlagSet, name string) bool { return strFlag(fs, name) == "true" }

func intFlag(fs *flag.FlagSet, name string) int { return intFlagOr(fs, name, 0) }

func intFlagOr(fs *flag.FlagSet, name string, def int) int {
	s := strFlag(fs, name)
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		return def
	}
	return n
}

func strSliceFlag(fs *flag.FlagSet, name string) []string {
	s := strFlag(fs, name)
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
}
