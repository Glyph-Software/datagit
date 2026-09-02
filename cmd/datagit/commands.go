package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Glyph-Software/datagit/internal/core"
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
