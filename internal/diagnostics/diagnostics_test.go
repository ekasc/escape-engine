package diagnostics

import (
	"bytes"
	"strings"
	"testing"
)

func TestRecorderKeepsTheMostRecentTurns(t *testing.T) {
	r := NewRecorder()
	for i := 0; i < defaultKeep+10; i++ {
		r.Record(Turn{TotalMs: int64(i)})
	}
	snap := r.Snapshot()
	if len(snap.Turns) != defaultKeep {
		t.Fatalf("expected the window to hold %d turns, got %d", defaultKeep, len(snap.Turns))
	}
	// The oldest turns are the ones dropped, so the newest must be last.
	if got := snap.Turns[len(snap.Turns)-1].TotalMs; got != int64(defaultKeep+9) {
		t.Fatalf("newest turn = %v, want %v", got, defaultKeep+9)
	}
}

// The percentiles are the number worth watching. A single slow turn is noise;
// a p95 that moved is a regression.
func TestSnapshotPercentiles(t *testing.T) {
	r := NewRecorder()
	// Exactly one window, so the recorded set is what the test reasons about.
	for i := 1; i <= defaultKeep; i++ {
		r.Record(Turn{FirstTokenMs: int64(i)})
	}
	snap := r.Snapshot()
	// Nearest-rank over 1..50: p50 is the 25th value, p95 the 47th.
	if snap.FirstTokenP50 != 25 {
		t.Fatalf("p50 = %dms, want 25ms", snap.FirstTokenP50)
	}
	if snap.FirstTokenP95 != 47 {
		t.Fatalf("p95 = %dms, want 47ms", snap.FirstTokenP95)
	}
}

func TestSnapshotReportsTheLastError(t *testing.T) {
	r := NewRecorder()
	r.Record(Turn{FirstTokenMs: 1})
	r.Record(Turn{Error: "provider status 400"})
	if got := r.Snapshot().LastError; got != "provider status 400" {
		t.Fatalf("last error = %q", got)
	}
}

func TestEmptyRecorderIsUsable(t *testing.T) {
	snap := NewRecorder().Snapshot()
	if len(snap.Turns) != 0 {
		t.Fatal("a new recorder should hold no turns")
	}
	if snap.FirstTokenP95 != 0 {
		t.Fatal("percentiles over no turns should be zero, not a panic")
	}
}

// A nil recorder must not panic. The agent always has one, but a zero-value
// Agent in a test should not take the process down.
func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Record(Turn{})
	if _, ok := r.Last(); ok {
		t.Fatal("a nil recorder has no turns")
	}
	if s := r.Snapshot(); len(s.Turns) != 0 {
		t.Fatal("a nil recorder reports nothing")
	}
}

func TestWriteReportNamesThePhasesAndTheModel(t *testing.T) {
	var buf bytes.Buffer
	WriteReport(&buf, Snapshot{
		Provider:      "opencode-go",
		Model:         "space-bunny-free",
		Thinking:      "max",
		Turns:         []Turn{{HistoryReadMs: 12, BuildMs: 3, FirstTokenMs: 900, TotalMs: 950, Iterations: 2}},
		FirstTokenP50: 900,
		FirstTokenP95: 900,
	})
	out := buf.String()
	for _, want := range []string{"opencode-go", "space-bunny-free", "1st token", "read", "build", "900ms", "p50", "p95"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report is missing %q:\n%s", want, out)
		}
	}
}

// A turn that never completed is the case someone opens diagnostics to find,
// so it has to read as a failure rather than as a fast turn.
func TestWriteReportSurfacesAFailedTurn(t *testing.T) {
	var buf bytes.Buffer
	WriteReport(&buf, Snapshot{Turns: []Turn{{Error: "error", TotalMs: 1000}}})
	if !strings.Contains(buf.String(), "error") {
		t.Fatalf("a failed turn must be visible in the report:\n%s", buf.String())
	}
}
