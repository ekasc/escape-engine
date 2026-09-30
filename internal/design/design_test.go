package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func goodArtifacts() []artifact {
	return []artifact{
		Qualify{Goal: "a landing page for a Go RPC engine", Audience: "backend developers", Assumptions: []string{"single page"}},
		Brief{Summary: "one page", Sections: []string{"hero", "features"}, Mood: "calm"},
		Brand{Name: "Escape", Palette: []string{"#0b0d10", "#3c4043", "#8ab4f8"}},
		Page{Route: "/", Blocks: []string{"hero", "cta"}, Copy: []string{"Ship it."}},
		Assets{Path: "public/hero.png", Alt: "the terminal", Kind: "screenshot"},
		Build{Files: []string{"index.html", "style.css"}},
	}
}

func TestPositionAdvancesAsArtifactsLand(t *testing.T) {
	s := newStore(t)

	pos, err := s.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseQualify {
		t.Fatalf("position = %q, want %q", pos, PhaseQualify)
	}

	for i, a := range goodArtifacts() {
		if err := s.Save(a); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		pos, err = s.Position()
		if err != nil {
			t.Fatal(err)
		}
		want := phases[i+1].phase
		if pos != want {
			t.Fatalf("after writing %s position = %q, want %q", a.phase(), pos, want)
		}
	}
}

func TestSaveRejectsInvalidArtifactAndLeavesStoreUntouched(t *testing.T) {
	s := newStore(t)
	bad := Brief{Summary: "", Sections: []string{"hero"}}

	err := s.Save(bad)
	if err == nil {
		t.Fatal("saved an invalid brief")
	}
	var invalid *InvalidError
	if !asInvalid(err, &invalid) {
		t.Fatalf("error = %v, want an *InvalidError", err)
	}
	if invalid.Phase != PhaseBrief || invalid.Field != "summary" {
		t.Errorf("error = %+v, want it to name brief.summary", invalid)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "brief.json")); !os.IsNotExist(err) {
		t.Error("a rejected artifact was written anyway")
	}
}

func asInvalid(err error, target **InvalidError) bool {
	for err != nil {
		if v, ok := err.(*InvalidError); ok {
			*target = v
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestPositionIgnoresAnInvalidArtifact(t *testing.T) {
	s := newStore(t)
	for _, a := range goodArtifacts() {
		if err := s.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt a completed phase the way a crash mid-write would.
	if err := os.WriteFile(filepath.Join(s.Dir(), "brand.json"), []byte(`{"name":"","palette":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pos, err := s.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseBrand {
		t.Errorf("position = %q, want %q for a phase whose artifact no longer validates", pos, PhaseBrand)
	}
}

func TestRepairsCountPassesAndBudgetIsBounded(t *testing.T) {
	s := newStore(t)
	finish(s, t)

	for pass := 0; pass <= MaxRepairs; pass++ {
		r := Review{Pass: pass, Verdict: VerdictRepair, From: PhasePage, Why: "hero is cramped"}
		if err := s.Save(r); err != nil {
			t.Fatalf("save review %d: %v", pass, err)
		}
		got, err := s.Repairs()
		if err != nil {
			t.Fatal(err)
		}
		if got != pass {
			t.Errorf("after review %d Repairs() = %d", pass, got)
		}
	}

	over := Review{Pass: MaxRepairs + 1, Verdict: VerdictRepair, From: PhasePage, Why: "still wrong"}
	if err := s.Save(over); err == nil {
		t.Error("accepted a review past the repair budget")
	}
	if spent, _ := s.Repairs(); spent < MaxRepairs {
		t.Errorf("Repairs() = %d after writing the final allowed pass, want %d", spent, MaxRepairs)
	}
}

func TestDropRewindsPositionAndKeepsTheRepairRecord(t *testing.T) {
	s := newStore(t)
	finish(s, t)
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
		t.Errorf("position = %q, want %q after a repair rewound to it", pos, PhasePage)
	}
	repairs, err := s.Repairs()
	if err != nil {
		t.Fatal(err)
	}
	if repairs != 0 {
		t.Errorf("Repairs() = %d, want 0 to survive the rewind", repairs)
	}
	// brief and brand precede page, so a page-level repair must not redo them.
	if _, err := os.Stat(filepath.Join(s.Dir(), "brief.json")); err != nil {
		t.Error("dropping from page removed the brief")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "page.json")); !os.IsNotExist(err) {
		t.Error("dropping from page kept the page artifact")
	}
}

func TestRunResumesAfterBeingKilled(t *testing.T) {
	s := newStore(t)
	finish(s, t)

	// A second store over the same directory is what a restarted process sees.
	resumed, err := NewStore(filepath.Dir(filepath.Dir(filepath.Dir(s.Dir()))), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	pos, err := resumed.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhasePreview {
		t.Errorf("position after restart = %q, want %q", pos, PhasePreview)
	}
}

func TestCapturePhaseNeedsBothShotsAtTheRightSize(t *testing.T) {
	s := newStore(t)
	for _, a := range goodArtifacts() {
		if err := s.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	pos, _ := s.Position()
	if pos != PhasePreview {
		t.Fatalf("position = %q, want %q", pos, PhasePreview)
	}
	ok, err := s.captured()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("capture reported done with no screenshots on disk")
	}

	if err := s.Drop(PhaseCapture); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureHostRefusesAnythingButLoopback(t *testing.T) {
	for _, target := range []string{
		"http://127.0.0.1:5173",
		"http://localhost:5173",
		"http://[::1]:5173",
	} {
		if _, err := captureHost(target); err != nil {
			t.Errorf("captureHost(%q) = %v, want accepted", target, err)
		}
	}
	for _, target := range []string{
		"https://example.com",
		"http://10.0.0.5:5173",
		"file:///etc/passwd",
		"ftp://127.0.0.1",
		"http://127.0.0.1.evil.com",
		"",
	} {
		if host, err := captureHost(target); err == nil {
			t.Errorf("captureHost(%q) = %q, want refused", target, host)
		}
	}
}

func TestNewStoreRejectsSessionIDsThatAreNotOneSegment(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "../escape", "/abs"} {
		if _, err := NewStore(root, id); err == nil {
			t.Errorf("NewStore accepted session id %q", id)
		}
	}
	if _, err := NewStore(root, "ok-1"); err != nil {
		t.Errorf("NewStore rejected a valid session id: %v", err)
	}
}

func finish(s *Store, t *testing.T) {
	t.Helper()
	for _, a := range goodArtifacts() {
		if err := s.Save(a); err != nil {
			t.Fatal(err)
		}
	}
}

func TestViewportNamesSortByNothingInParticular(t *testing.T) {
	if !strings.HasPrefix(shotFile(Desktop), "shot-1440x1000") {
		t.Errorf("desktop shot file = %q", shotFile(Desktop))
	}
	if !strings.HasSuffix(shotFile(Mobile), ".png") {
		t.Errorf("mobile shot file = %q", shotFile(Mobile))
	}
}
