package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Glyph-Software/datagit/internal/adapter"

	"github.com/Glyph-Software/datagit/internal/core"
	"github.com/Glyph-Software/datagit/internal/crypto"
	"github.com/Glyph-Software/datagit/internal/store"
)

// --- branching ---

func cmdBranch(e *env2) error {
	if len(e.args) == 0 || e.args[0] == "list" {
		refs, err := e.st.ListRefs(e.ctx, e.repo)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tNAME\tHEAD\tPARENT\tDEPTH\tPROTECTED")
		for _, r := range refs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%v\n",
				r.Kind, r.Name, r.Head.Short(), r.Parent, len(r.Chain), r.Protected)
		}
		return w.Flush()
	}
	switch e.args[0] {
	case "create":
		if len(e.args) < 2 {
			return fmt.Errorf("usage: datagit branch create <name> [--from <branch>]")
		}
		from := strFlag(e.fs, "from")
		if from == "" {
			from = store.DefaultBranch
		}
		if e.g.author == "" {
			return fmt.Errorf("--author is required")
		}
		r, err := e.st.CreateBranch(e.ctx, e.repo, e.args[1], from, e.g.author)
		if err != nil {
			return err
		}
		fmt.Printf("created branch %s from %s (chain depth %d, no data copied)\n",
			r.Name, from, len(r.Chain))
		return nil
	case "delete":
		if len(e.args) < 2 {
			return fmt.Errorf("usage: datagit branch delete <name>")
		}
		if err := e.st.DeleteBranch(e.ctx, e.repo, e.args[1]); err != nil {
			return err
		}
		fmt.Printf("deleted branch %s (its versions are reclaimable by gc for %v)\n",
			e.args[1], store.GCGracePeriod)
		return nil
	case "protect":
		if len(e.args) < 2 {
			return fmt.Errorf("usage: datagit branch protect <name> [--approvals n]")
		}
		n := intFlagOr(e.fs, "approvals", 1)
		if err := e.st.SetBranchProtection(e.ctx, e.repo, e.args[1],
			store.BranchProtection{Protected: true, MinApprovals: n}); err != nil {
			return err
		}
		fmt.Printf("branch %s now requires %d approval(s); self-approval does not count\n", e.args[1], n)
		return nil
	}
	return fmt.Errorf("unknown branch subcommand %q", e.args[0])
}

func cmdMerge(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit merge <from-branch> --table <table> [--into <branch>]")
	}
	if e.g.author == "" {
		return fmt.Errorf("--author is required")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	into := strFlag(e.fs, "into")
	if into == "" {
		into = store.DefaultBranch
	}
	res, err := e.st.Merge(e.ctx, e.repo, t, e.args[0], into, e.g.author, strFlag(e.fs, "m"), true)
	if err != nil {
		return err
	}
	return reportMerge(t, res)
}

func reportMerge(t *store.Table, res *store.MergeResult) error {
	if !res.Clean {
		fmt.Printf("merge is CONFLICTED (%d conflict(s)); nothing was applied\n", len(res.Conflicts))
		for _, c := range res.Conflicts {
			name := ""
			if c.HasCol {
				if cc, ok := t.Column(c.Col); ok {
					name = " " + cc.Name
				}
			}
			fmt.Printf("  %s%s  base=%s ours=%s theirs=%s\n", c.Kind, name, c.Base, c.Ours, c.Theirs)
		}
		return fmt.Errorf("resolve the conflicts and retry")
	}
	fmt.Printf("%s  merged cleanly, %d row(s) applied\n", res.Commit.Short(), res.Applied)
	return nil
}

func cmdUpdateFrom(e *env2) error {
	if e.g.author == "" {
		return fmt.Errorf("--author is required")
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	res, err := e.st.UpdateFromParent(e.ctx, e.repo, t, e.g.branch, e.g.author)
	if err != nil {
		return err
	}
	return reportMerge(t, res)
}

// --- proposals ---

func cmdProposal(e *env2) error {
	if len(e.args) == 0 || e.args[0] == "list" {
		ps, err := e.st.ListProposals(e.ctx, e.repo)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tFROM\tINTO\tAUTHOR\tTITLE")
		for _, p := range ps {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.State, p.From, p.Into, p.CreatedBy, p.Title)
		}
		return w.Flush()
	}
	if e.g.author == "" {
		return fmt.Errorf("--author is required")
	}
	switch e.args[0] {
	case "create":
		from := requireFlag(e.fs, "from")
		into := strFlag(e.fs, "into")
		if into == "" {
			into = store.DefaultBranch
		}
		p, err := e.st.CreateProposal(e.ctx, e.repo, from, into,
			requireFlag(e.fs, "title"), strFlag(e.fs, "m"), e.g.author)
		if err != nil {
			return err
		}
		fmt.Printf("proposal #%d: %s -> %s\n", p.ID, from, into)
		return nil
	case "approve", "comment", "reject":
		id := int64(intFlag(e.fs, "id"))
		kind := map[string]string{"approve": "approve", "comment": "comment", "reject": "request_changes"}[e.args[0]]
		if err := e.st.AddReview(e.ctx, e.repo, id, kind, strFlag(e.fs, "m"), e.g.author); err != nil {
			return err
		}
		fmt.Printf("recorded %s on proposal #%d\n", kind, id)
		return nil
	case "merge":
		id := int64(intFlag(e.fs, "id"))
		t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
		if err != nil {
			return err
		}
		res, err := e.st.MergeProposal(e.ctx, e.repo, t, id, e.g.author)
		if err != nil {
			return err
		}
		return reportMerge(t, res)
	case "conflicts":
		id := int64(intFlag(e.fs, "id"))
		t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
		if err != nil {
			return err
		}
		cs, err := e.st.ListConflicts(e.ctx, t, id)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tCOLUMN\tBASE\tOURS\tTHEIRS\tRESOLVED")
		for _, c := range cs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%v\n",
				c.ID, c.Kind, c.Column, c.Base, c.Ours, c.Theirs, c.Resolved)
		}
		return w.Flush()
	}
	return fmt.Errorf("unknown proposal subcommand %q", e.args[0])
}

// --- operations ---

func cmdMaterialize(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit materialize <branch> --into <schema>")
	}
	into := requireFlag(e.fs, "into")
	if err := e.st.Materialize(e.ctx, e.repo, e.args[0], into); err != nil {
		return err
	}
	fmt.Printf("materialized %s into schema %s — ordinary tables, unrestricted SQL,\n"+
		"a point-in-time copy that is not writable back into the branch\n", e.args[0], into)
	return nil
}

func cmdGC(e *env2) error {
	rep, err := e.st.GC(e.ctx, e.repo)
	if err != nil {
		return err
	}
	fmt.Printf("reclaimed %d orphan version(s), reaped %d expired session(s)\n",
		rep.OrphanVersions, rep.SessionsReaped)
	return nil
}

func cmdPrune(e *env2) error {
	t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	rep, err := e.st.Prune(e.ctx, e.repo, t, store.RetentionPolicy{
		KeepDays: intFlag(e.fs, "keep-days"), KeepCommits: intFlag(e.fs, "keep-commits"),
	})
	if err != nil {
		return err
	}
	fmt.Printf("removed %d version(s); %d commit(s) were protected from pruning\n",
		rep.VersionsRemoved, rep.CommitsProtected)
	return nil
}

func cmdPurge(e *env2) error {
	if e.g.author == "" {
		return fmt.Errorf("--author is required")
	}
	t, pk, err := tableAndPK(e)
	if err != nil {
		return err
	}
	reason := strFlag(e.fs, "reason")
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required: purge is audited and irreversible")
	}
	rec, err := e.st.Purge(e.ctx, e.repo, t, pk, reason, e.g.author)
	if err != nil {
		return err
	}
	fmt.Printf("purged %d version(s); %d commit(s) marked integrity=purged\n",
		rec.VersionsRemoved, rec.CommitsMarked)
	fmt.Println("the hash chain is deliberately broken for those commits and NOT re-hashed,")
	fmt.Println("so an authorized erasure stays distinguishable from tampering")
	return nil
}

// requireFlag returns a flag's value, or the empty string. Callers that need it
// present check the result; keeping this total avoids a panic path in the CLI.
func requireFlag(fs *flag.FlagSet, name string) string { return strFlag(fs, name) }

var _ = core.OpUpdate

// --- Schema (§10.4) ----------------------------------------------------------

func cmdSchema(e *env2) error {
	sub := "show"
	if len(e.args) > 0 {
		sub = e.args[0]
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	branch := branchOr(e, store.DefaultBranch)

	switch sub {
	case "show":
		v, err := e.st.LoadSchema(e.ctx, e.repo, t, branch)
		if err != nil {
			return err
		}
		fmt.Printf("%s @ %s — schema epoch %d\n", t.Physical, branch, v.Epoch)
		for _, c := range v.Columns {
			null := "NOT NULL"
			if c.Nullable {
				null = "NULL"
			}
			fmt.Printf("  %-4d %-20s %-20s %s\n", c.ID, c.Name, c.SQLType, null)
		}
		for id, at := range v.Dropped {
			fmt.Printf("  %-4d %-20s dropped at epoch %d (history still readable)\n",
				id, "-", at)
		}
		return nil

	case "add-column", "drop-column":
		if branch == store.DefaultBranch {
			return fmt.Errorf(
				"%s is what direct readers compiled against: change the shape on a "+
					"branch and propose it, so the change arrives as a migration plan "+
					"with a rollout window (§10.4)", store.DefaultBranch)
		}
		cur, err := e.st.LoadSchema(e.ctx, e.repo, t, branch)
		if err != nil {
			return err
		}
		name := requireFlag(e.fs, "column")
		want := append([]adapter.Column(nil), cur.Columns...)

		if sub == "add-column" {
			sqlType := requireFlag(e.fs, "type")
			var next core.ColID
			for _, c := range want {
				if c.ID > next {
					next = c.ID
				}
			}
			for id := range cur.Dropped {
				if id > next {
					next = id
				}
			}
			want = append(want, adapter.Column{
				ID: next + 1, Name: name, SQLType: sqlType, Nullable: true,
			})
		} else {
			var kept []adapter.Column
			for _, c := range want {
				if c.Name != name {
					kept = append(kept, c)
				}
			}
			if len(kept) == len(want) {
				return fmt.Errorf("%s has no column %q", t.Physical, name)
			}
			want = kept
		}

		res, err := e.st.AlterBranchSchema(e.ctx, e.repo, t, branch, want, e.g.author)
		if err != nil {
			return err
		}
		fmt.Printf("%s @ %s is now schema epoch %d (%d change(s))\n",
			t.Physical, branch, res.Epoch, len(res.Changes))
		for _, name := range res.Forked {
			fmt.Printf("  %s narrowed, so it forked to a new column id: old versions "+
				"stay readable in the old column (§10.5 rule 3)\n", name)
		}
		fmt.Printf("the live table is unchanged; propose the branch to produce a " +
			"migration plan\n")
		return nil
	}
	return fmt.Errorf("unknown schema subcommand %q", sub)
}

func cmdMigration(e *env2) error {
	sub := "list"
	if len(e.args) > 0 {
		sub = e.args[0]
	}
	switch sub {
	case "list":
		plans, err := e.st.ListMigrationPlans(e.ctx, e.repo, "pending", "applying", "failed")
		if err != nil {
			return err
		}
		if len(plans) == 0 {
			fmt.Println("no migration plans awaiting apply")
			return nil
		}
		for _, p := range plans {
			fmt.Printf("%-6d %-10s epoch %d  by %s\n", p.ID, p.State, p.TargetEpoch, p.CreatedBy)
		}
		return nil

	case "show", "apply":
		if len(e.args) < 2 {
			return fmt.Errorf("usage: datagit migration %s <id>", sub)
		}
		id, err := strconv.ParseInt(e.args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("migration id must be a number: %w", err)
		}
		if sub == "show" {
			p, err := e.st.LoadMigrationPlan(e.ctx, e.repo, id)
			if err != nil {
				return err
			}
			fmt.Printf("migration plan %d (%s), target epoch %d\n", p.ID, p.State, p.TargetEpoch)
			for _, op := range p.Ops {
				fmt.Printf("  %d. [%s] %s\n", op.Ordinal, op.Kind, op.SQL)
			}
			for _, r := range p.Confirm {
				fmt.Printf("  ! %s\n", r)
			}
			if len(p.Confirm) > 0 {
				fmt.Println("  apply with --confirm once readers can tolerate it")
			}
			return nil
		}
		p, err := e.st.ApplyMigrationPlan(e.ctx, e.repo, id, boolFlag(e.fs, "confirm"), e.g.author)
		if err != nil {
			return err
		}
		fmt.Printf("applied migration plan %d — the live table now has the merged shape\n", p.ID)
		return nil
	}
	return fmt.Errorf("unknown migration subcommand %q", sub)
}

// branchOr returns the --branch flag, or a default when it was not given.
func branchOr(e *env2, def string) string {
	if e.g.branch != "" {
		return e.g.branch
	}
	return def
}

// --- Compliance (§13.3) ------------------------------------------------------

func cmdPII(e *env2) error {
	sub := "list"
	if len(e.args) > 0 {
		sub = e.args[0]
	}
	t, err := e.st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		cols, err := e.st.PIIColumns(e.ctx, t)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			fmt.Printf("%s designates no PII columns\n", t.Physical)
			return nil
		}
		for _, c := range cols {
			fmt.Printf("  %-20s subject from %s\n", c.Name, c.SubjectName)
		}
		fmt.Println("crypto-shredding protects these columns and no others: personal data")
		fmt.Println("in an undesignated field is not covered by it")
		return nil

	case "designate":
		st, err := withEnvelope(e)
		if err != nil {
			return err
		}
		col := requireFlag(e.fs, "column")
		subj := requireFlag(e.fs, "subject-column")
		if err := st.DesignatePII(e.ctx, e.repo, t, col, subj, e.g.author); err != nil {
			return err
		}
		fmt.Printf("%s.%s is PII, with the data subject read from %s\n", t.Physical, col, subj)
		fmt.Println("existing history for that column has been sealed")
		return nil
	}
	return fmt.Errorf("unknown pii subcommand %q", sub)
}

func cmdErase(e *env2) error {
	if len(e.args) < 1 {
		return fmt.Errorf("usage: datagit erase <subject> --table <t> --reason <why>")
	}
	st, err := withEnvelope(e)
	if err != nil {
		return err
	}
	t, err := st.LoadTable(e.ctx, e.repo, requireFlag(e.fs, "table"))
	if err != nil {
		return err
	}
	reason := requireFlag(e.fs, "reason")
	if reason == "" {
		return fmt.Errorf("--reason is required: an erasure is a recorded act")
	}
	rep, err := st.EraseSubject(e.ctx, e.repo, t, e.args[0], reason, e.g.author)
	if err != nil {
		return err
	}
	fmt.Printf("erased data subject %s\n", rep.Subject)
	fmt.Printf("  %d current row(s) deleted by commit %s\n", rep.RowsErased, rep.Commit.Short())
	fmt.Printf("  key destroyed: every historical value for them is now unreadable\n")
	fmt.Printf("  the hash chain is untouched and still verifies\n")
	return nil
}

// withEnvelope returns a store with crypto-shredding enabled, loading the
// key-encryption key from a file.
//
// In production this key lives in a KMS and never enters the process. The file
// form exists so the mechanism can be exercised without one, and it is worth
// being blunt: a KEK on disk beside the database protects against very little.
func withEnvelope(e *env2) (*store.Store, error) {
	path := requireFlag(e.fs, "kek-file")
	if path == "" {
		return nil, fmt.Errorf(
			"--kek-file (or DATAGIT_KEK_FILE) is required: crypto-shredding needs a " +
				"key-encryption key, and a lost key is indistinguishable from an erased " +
				"one, so its durability is the key store's problem (§13.3)")
	}
	kek, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the key-encryption key: %w", err)
	}
	if len(kek) > crypto.KeyLen {
		kek = kek[:crypto.KeyLen]
	}
	env, err := crypto.NewLocalEnvelope(kek)
	if err != nil {
		return nil, err
	}
	return e.st.WithEnvelope(env), nil
}
