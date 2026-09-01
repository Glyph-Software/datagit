// Package property is the differential test harness: PLAN.md's primary
// correctness evidence (W1).
//
// It drives the reference model (internal/model, snapshot-based) and the real
// engine (internal/engine, interval-and-mask-based) through identical random
// operation sequences and asserts they never disagree on resolved state, merge
// results, or conflict sets — plus the standing invariants in
// PLAN.md §Verification.
//
// Run a longer sweep with:
//
//	go test ./test/property -run TestDifferential -sequences 200000
package property

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

var (
	flagSequences = flag.Int("sequences", 2000, "number of random operation sequences")
	flagOps       = flag.Int("ops", 40, "operations per sequence")
	flagSeed      = flag.Int64("seed", 1, "base seed")
)

// runSequence applies a whole sequence, checking invariants after every step.
// It returns the failing step index and error, or -1 on success.
func runSequence(ops []Op) (int, error) {
	o := newOracle()
	for i, op := range ops {
		if err := o.apply(op); err != nil {
			return i, err
		}
		if err := o.check(); err != nil {
			return i, err
		}
	}
	return -1, nil
}

// shrink removes operations one at a time for as long as the failure survives,
// so a report names the shortest sequence that still reproduces.
func shrink(ops []Op) []Op {
	cur := ops
	changed := true
	for changed {
		changed = false
		for i := 0; i < len(cur); i++ {
			cand := make([]Op, 0, len(cur)-1)
			cand = append(cand, cur[:i]...)
			cand = append(cand, cur[i+1:]...)
			if _, err := runSequence(cand); err != nil {
				cur = cand
				changed = true
				break
			}
		}
	}
	return cur
}

func TestDifferential(t *testing.T) {
	failures := 0
	for s := 0; s < *flagSequences; s++ {
		seed := *flagSeed + int64(s)
		ops := GenSequence(rand.New(rand.NewSource(seed)), *flagOps)
		step, err := runSequence(ops)
		if err == nil {
			continue
		}
		failures++
		min := shrink(ops)
		t.Errorf("seed %d failed at step %d: %v\n\nshrunk to %d ops:\n%s",
			seed, step, err, len(min), FormatSequence(min))
		if failures >= 3 {
			t.Fatalf("stopping after %d failures", failures)
		}
	}
	t.Logf("%d sequences x %d ops: no divergence between model and engine",
		*flagSequences, *flagOps)
}

// TestSeedCorpus replays sequences that previously failed. PLAN.md: failures are
// minimized and added to the corpus permanently.
func TestSeedCorpus(t *testing.T) {
	seeds := corpusSeeds(t)
	if len(seeds) == 0 {
		t.Skip("no seed corpus yet")
	}
	for _, seed := range seeds {
		ops := GenSequence(rand.New(rand.NewSource(seed)), *flagOps)
		if step, err := runSequence(ops); err != nil {
			t.Errorf("corpus seed %d regressed at step %d: %v", seed, step, err)
		}
	}
}

func corpusSeeds(t *testing.T) []int64 {
	t.Helper()
	data, err := os.ReadFile("testdata/corpus.txt")
	if err != nil {
		return nil
	}
	var seeds []int64
	for _, line := range splitLines(string(data)) {
		var n int64
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil {
			seeds = append(seeds, n)
		}
	}
	return seeds
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// BenchmarkSequence measures harness throughput, which sets how many sequences
// a CI run can afford.
func BenchmarkSequence(b *testing.B) {
	ops := GenSequence(rand.New(rand.NewSource(1)), *flagOps)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runSequence(ops); err != nil {
			b.Fatal(err)
		}
	}
}
