package design

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeShots puts a real, decodable PNG at each viewport path. Store.captured
// decodes the header, so an empty file would not count as a capture.
func writeShots(t *testing.T, s *Store) {
	t.Helper()
	for _, v := range []Viewport{Desktop, Mobile} {
		img := image.NewRGBA(image.Rect(0, 0, v.Width, v.Height))
		f, err := os.Create(filepath.Join(s.dir, shotFile(v)))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A run whose repair budget is spent must not get a new budget by restarting.
//
// Position is derived from artifacts on disk, and the last review of a spent
// run still says "repair", so a restarted runner is sent back to review. If it
// starts that pass counting from zero it can repair forever, and it overwrites
// review-0.json each time, so the history of what was tried is destroyed too.
//
// The counter has to come from the store, which is the only thing that survives.
func TestResumedRunnerInheritsTheSpentRepairBudget(t *testing.T) {
	s := newStore(t)
	finish(s, t)
	writeShots(t, s)

	// Three reviews, every one asking for a repair: the budget is spent.
	for pass := 0; pass <= MaxRepairs; pass++ {
		rev := Review{
			Pass:    pass,
			Verdict: VerdictRepair,
			From:    PhasePage,
			Why:     "the hero does not match the reference",
		}
		if err := s.Save(rev); err != nil {
			t.Fatalf("save review %d: %v", pass, err)
		}
	}

	repairs, err := s.Repairs()
	if err != nil {
		t.Fatal(err)
	}
	if repairs != MaxRepairs {
		t.Fatalf("store repairs = %d, want %d", repairs, MaxRepairs)
	}

	// A restarted runner reads position from disk. A spent run is finished, not
	// stuck asking for another pass, so it must not be sent back to review.
	resumed, err := NewStore(s.root, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	pos, err := resumed.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseDone {
		t.Fatalf("position of a spent run = %q, want %q", pos, PhaseDone)
	}

	// The counter must not be reset by the restart either. Its exact value is
	// not what matters for a spent run — Position already keeps it out of
	// review — but a value below the budget would mean a fresh one somewhere.
	r, err := NewRunner(nil, Config{Request: "a landing page for a Go RPC engine", Root: s.root, SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.repairs < MaxRepairs {
		t.Errorf("resumed runner repairs = %d, want at least %d; a restart would grant a fresh budget",
			r.repairs, MaxRepairs)
	}
}

// The other half: a run that crashed *after* a review asked for a repair but
// before the redo finished. Position walks back to the dropped phase, and the
// next review it writes must be pass 2, not pass 1.
func TestResumedRunnerNumbersItsNextReviewAfterTheOneItSpent(t *testing.T) {
	s := newStore(t)
	finish(s, t)
	writeShots(t, s)

	if err := s.Save(Review{Pass: 0, Verdict: VerdictRepair, From: PhasePage, Why: "hero is cramped"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(PhasePage); err != nil {
		t.Fatal(err)
	}

	pos, err := s.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhasePage {
		t.Fatalf("position = %q, want %q", pos, PhasePage)
	}

	r, err := NewRunner(nil, Config{Request: "a landing page for a Go RPC engine", Root: s.root, SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.repairs != 1 {
		t.Errorf("resumed runner repairs = %d, want 1", r.repairs)
	}
}

// Every artifact type has value receivers, so both Review and *Review satisfy
// the artifact interface. A store that numbered only one of them would write
// "review.json" for the other — a name nothing counts, so the run would lose
// its repair history silently rather than fail.
func TestStoreNumbersAPointerReviewToo(t *testing.T) {
	s := newStore(t)
	rev := &Review{Pass: 1, Verdict: VerdictRepair, From: PhasePage, Why: "hero is cramped"}
	if err := s.Save(rev); err != nil {
		t.Fatal(err)
	}
	passes, err := s.Reviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 1 || passes[0] != 1 {
		t.Errorf("reviews on disk = %v, want [1]; a pointer review was not numbered per pass", passes)
	}
}
