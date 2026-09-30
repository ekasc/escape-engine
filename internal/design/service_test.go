package design

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A run that cannot accept a request has nothing to qualify. The failure is
// loud here rather than a model being asked whether an unnamed request is a
// design task.
func TestRunnerRefusesARunWithNoRequest(t *testing.T) {
	dir := t.TempDir()
	for _, cfg := range []Config{
		{Root: dir, SessionID: "sess-1"},
		{Root: dir, SessionID: "sess-1", Request: "   \n\t "},
	} {
		if _, err := NewRunner(nil, cfg); err == nil {
			t.Errorf("NewRunner(%+v) accepted a run with no request", cfg)
		}
	}
}

// The request has to reach the model, not just the constructor. Qualify is the
// only phase that reads it, because every later phase learns what was asked
// from the artifacts in front of it.
func TestQualifyPromptCarriesTheRequest(t *testing.T) {
	s := newStore(t)
	const request = "a pricing page for a Postgres service"
	prompt := phasePrompt(s, PhaseQualify, "site", request, nil)
	if !strings.Contains(prompt, request) {
		t.Errorf("qualify prompt does not carry the request:\n%s", prompt)
	}
	if !strings.Contains(prompt, "This phase: qualify") {
		t.Error("qualify prompt lost its phase header")
	}

	// Later phases read the artifacts instead, so restating the request there
	// would be the same instruction three times over.
	later := phasePrompt(s, PhaseBuild, "site", request, nil)
	if strings.Contains(later, request) {
		t.Error("build prompt restates the request; it should read the artifacts")
	}
}

// Two front doors reach the same run, so the service has to hold exactly one at
// a time and hand progress to a caller that subscribes late without losing the
// phases it slept through.
func TestServiceRunsOneRunPerKeyAndReplaysProgress(t *testing.T) {
	m := newScriptedModel(t, "site", baseReplies())
	ag, dir := newRunAgent(t, m)
	cfg := Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "svc-1", SiteDir: m.siteDir, Tool: ToolStatic}

	svc := NewService()
	run, err := svc.Start(dir, "svc-1", "site", cfg.Request, func() (*Runner, error) {
		return NewRunner(ag, cfg)
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Start(dir, "svc-1", "site", cfg.Request, func() (*Runner, error) {
		return NewRunner(ag, cfg)
	}); err == nil {
		t.Error("started a second run for a key that already had one")
	} else if !errors.Is(err, ErrBusy) {
		t.Errorf("second start error = %v, want ErrBusy", err)
	}

	// Subscribe only after the run is well under way. Phases already reported
	// must still arrive, or a caller that attached late would see a run that
	// appears to begin at whatever phase it happened to catch.
	time.Sleep(150 * time.Millisecond)
	first := run.Updates()
	seen := 0
	for u := range first {
		if u.Result != nil {
			break
		}
		seen++
	}
	if seen == 0 {
		t.Error("a late subscriber received no progress at all")
	}

	<-run.Done()
	if len(run.Progress()) == 0 {
		t.Error("run recorded no progress")
	}
	if svc.Busy("svc-1") {
		t.Error("service still reports the key busy after the run finished")
	}

	// And a caller that subscribes after the run has finished still gets the
	// whole story, not an empty channel.
	after := 0
	for u := range run.Updates() {
		if u.Result != nil {
			continue
		}
		after++
	}
	if after == 0 {
		t.Error("subscribing to a finished run delivered no progress")
	}
}
