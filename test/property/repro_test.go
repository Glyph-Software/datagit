package property

import (
	"testing"
)

// TestReproShrunk replays specific minimized sequences that the fuzzer found.
//
// Unlike TestSeedCorpus (which replays whole generated sequences by seed), these
// are the shrunk forms, transcribed as literals so the case survives any change
// to the generator's weighting.
func TestReproShrunk(t *testing.T) {
	cases := []struct {
		name string
		ops  []Op
	}{
		{
			// Cross-branch merge where the source is NOT on the target's chain:
			// b1 (child of main) merged into b5 (grandchild of main via b3),
			// after both have absorbed different amounts of main.
			name: "cross-branch merge with divergent fork points",
			ops: []Op{
				{Kind: KBranch, A: 0, B: 1},
				{Kind: KBranch, A: 1, B: 4},
				{Kind: KBranch, A: 0, B: 4},
				{Kind: KCommit, A: 0, B: 4, Rows: []RowOp{
					{Key: 1, Vals: [3]int{4, 3, -1}},
					{Key: 5, Vals: [3]int{-1, -1, 0}},
					{Key: 6, Del: true},
				}},
				{Kind: KUpdateFromParent, A: 3, B: 4},
				{Kind: KCommit, A: 0, B: 1, Rows: []RowOp{
					{Key: 5, Vals: [3]int{1, 3, 1}},
				}},
				{Kind: KUpdateFromParent, A: 1, B: 5},
				{Kind: KCommit, A: 4, B: 4, Rows: []RowOp{
					{Key: 5, Vals: [3]int{0, 3, 0}},
				}},
				{Kind: KBranch, A: 2, B: 3},
				{Kind: KBranch, A: 3, B: 3},
				{Kind: KMerge, A: 0, B: 5},
				{Kind: KUpdateWhere, A: 1, B: 0, FCol: 0, FVal: 1, ACol: 1, AVal: 2, AAdd: true},
				{Kind: KMerge, A: 1, B: 5},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if step, err := runSequence(tc.ops); err != nil {
				t.Fatalf("failed at step %d (%s): %v\n\nsequence:\n%s",
					step, tc.ops[step], err, FormatSequence(tc.ops))
			}
		})
	}
}

// TestReproTrace replays the same sequence printing state after every step, so a
// divergence can be traced to the operation that introduced it. Run with -v.
func TestReproTrace(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("run with -v to trace")
	}
	ops := []Op{
		{Kind: KBranch, A: 0, B: 1},
		{Kind: KBranch, A: 1, B: 4},
		{Kind: KBranch, A: 0, B: 4},
		{Kind: KCommit, A: 0, B: 4, Rows: []RowOp{
			{Key: 1, Vals: [3]int{4, 3, -1}},
			{Key: 5, Vals: [3]int{-1, -1, 0}},
			{Key: 6, Del: true},
		}},
		{Kind: KUpdateFromParent, A: 3, B: 4},
		{Kind: KCommit, A: 0, B: 1, Rows: []RowOp{{Key: 5, Vals: [3]int{1, 3, 1}}}},
		{Kind: KUpdateFromParent, A: 1, B: 5},
		{Kind: KCommit, A: 4, B: 4, Rows: []RowOp{{Key: 5, Vals: [3]int{0, 3, 0}}}},
		{Kind: KBranch, A: 2, B: 3},
		{Kind: KBranch, A: 3, B: 3},
		{Kind: KMerge, A: 0, B: 5},
		{Kind: KUpdateWhere, A: 1, B: 0, FCol: 0, FVal: 1, ACol: 1, AVal: 2, AAdd: true},
		{Kind: KMerge, A: 1, B: 5},
	}
	o := newOracle()
	for i, op := range ops {
		err := o.apply(op)
		t.Logf("step %2d  %s  (apply err: %v)", i, op, err)
		for _, b := range o.branches {
			mt, et := o.m.Resolve(b), o.e.Resolve(b)
			mark := "  "
			if !mt.Equal(et) {
				mark = "!!"
			}
			t.Logf("   %s %-4s head=%-4s fork=%-4s", mark, b, o.e.Head(b), o.e.ForkCommit(b))
			t.Logf("        model : %s", fmtTable(mt))
			if !mt.Equal(et) {
				t.Logf("        engine: %s", fmtTable(et))
			}
		}
		if err := o.check(); err != nil {
			t.Fatalf("step %d check failed: %v", i, err)
		}
	}
}
