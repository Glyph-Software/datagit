package obs

import (
	"testing"
	"time"
)

func TestSnapshotCountsWhatOperatorsAsk(t *testing.T) {
	m := New()
	m.CommitOK(3, 2*time.Millisecond)
	m.CommitOK(1, 5*time.Millisecond)
	m.CommitFailed()
	m.Merge(0, 40*time.Millisecond)
	m.Merge(2, 60*time.Millisecond)
	m.Read(3, time.Millisecond)
	m.Drift(4)
	m.SessionOpened()

	s := m.Snapshot()
	if s.Commits != 2 || s.CommitRows != 4 || s.CommitsFailed != 1 {
		t.Errorf("commit counters wrong: %+v", s)
	}
	if s.Merges != 2 || s.MergesConflicted != 1 {
		t.Errorf("merge counters wrong: %+v", s)
	}
	if s.DriftFindings != 4 || s.SessionsOpen != 1 {
		t.Errorf("drift/session counters wrong: %+v", s)
	}
	// Chain depth is tracked because branch reads degrade with it and the §18
	// cap only bounds the worst case.
	if s.ChainDepth[3] != 1 {
		t.Errorf("chain depth not recorded: %+v", s.ChainDepth)
	}
	if s.Latency["commit_seconds"].Count != 2 {
		t.Errorf("commit latency not recorded: %+v", s.Latency)
	}
}

func TestPercentilesAreMonotonic(t *testing.T) {
	m := New()
	for i := 0; i < 1000; i++ {
		m.CommitOK(1, time.Duration(i)*time.Microsecond)
	}
	l := m.Snapshot().Latency["commit_seconds"]
	if !(l.P50 <= l.P95 && l.P95 <= l.P99) {
		t.Errorf("percentiles are not ordered: %+v", l)
	}
}

func TestSessionsOpenGoesDown(t *testing.T) {
	m := New()
	m.SessionOpened()
	m.SessionOpened()
	m.SessionClosed()
	if got := m.Snapshot().SessionsOpen; got != 1 {
		t.Errorf("open sessions is %d, want 1", got)
	}
}
