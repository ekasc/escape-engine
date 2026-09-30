package design

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

// scriptedModel answers each phase with a valid artifact, and writes a real page
// when the build phase asks it to. It is a model that cooperates, which is the
// only thing this test is about: the pipeline, not the model's judgement.
type scriptedModel struct {
	t        *testing.T
	root     string
	siteDir  string
	replies  map[Phase][]string
	calls    map[Phase]int
	repaired bool
}

func newScriptedModel(t *testing.T, siteDir string, replies map[Phase][]string) *scriptedModel {
	return &scriptedModel{t: t, siteDir: siteDir, replies: replies, calls: map[Phase]int{}}
}

// reply returns the scripted answer for the nth time a phase is asked. Once the
// script runs out the last answer repeats, because a model asked the same
// question again does not start answering in prose.
func (m *scriptedModel) reply(p Phase) string {
	i := m.calls[p]
	m.calls[p]++
	list := m.replies[p]
	if len(list) == 0 {
		return ""
	}
	if i >= len(list) {
		return list[len(list)-1]
	}
	return list[i]
}

func (m *scriptedModel) handler(ctx context.Context, req provider.Request) ([]provider.Event, error) {
	if agent.IsNamingRequest(req) {
		return done("named"), nil
	}
	prompt := lastUserText(req)
	phase := phaseOfPrompt(prompt)

	if phase == PhaseBuild {
		// The build phase uses the agent's own tools, so the page really lands
		// on disk and the preview really has something to serve.
		page := "<!doctype html><html><head><meta charset='utf-8'><title>Built</title>" +
			"<style>body{margin:0;background:#0b0d10;color:#e8eaed;font-family:system-ui}" +
			"h1{font-size:48px;padding:64px 32px 0}p{padding:0 32px;color:#9aa0a6}</style></head>" +
			"<body><h1>Built by the scripted model</h1><p>Below the fold.</p></body></html>"
		if err := os.MkdirAll(filepath.Join(m.root, m.siteDir), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(m.root, m.siteDir, "index.html"), []byte(page), 0o644); err != nil {
			return nil, err
		}
		return done(`{"files":["index.html"],"notes":"one page"}`), nil
	}

	text := m.reply(phase)
	if text == "" {
		return done("no scripted reply for " + string(phase)), nil
	}
	// The reviewer has to actually read the screenshots, or the run would pass
	// without the image ever reaching the model.
	return done(text), nil
}

func done(text string) []provider.Event {
	return []provider.Event{{Kind: provider.EventText, Text: text}, {Kind: provider.EventDone, StopReason: "stop"}}
}

func lastUserText(req provider.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Text
		}
	}
	return ""
}

func phaseOfPrompt(prompt string) Phase {
	for _, p := range append(phaseOrder, PhaseBuild, PhaseReview) {
		if strings.Contains(prompt, "## This phase: "+string(p)) {
			return p
		}
	}
	return ""
}

func newRunAgent(t *testing.T, m *scriptedModel) (*agent.Agent, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.Open(filepath.Join(dir, "s.jsonl"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m.root = dir
	ag := agent.New(agent.Options{
		Store:    store,
		Provider: &provider.Fake{Handler: m.handler},
		Tools:    tools.Default(tools.Deps{Cwd: dir}),
		Cwd:      dir,
		Model:    "gpt-5.6-sol",
	})
	return ag, dir
}

const (
	qualifyReply = `{"goal":"a landing page for a Go RPC engine","audience":"backend developers","constraints":["dark","no framework"],"assumptions":["single page"]}`
	briefReply   = "Here you go.\n\n```json\n" + `{"summary":"one page","sections":["hero","features"],"mood":"calm","must_avoid":["generic gradients"]}` + "\n```"
	brandReply   = `{"name":"Escape","palette":["#0b0d10","#8ab4f8","#e8eaed"],"type_scale":"48px hero, 16px body","mood":["calm","precise"]}`
	pageReply    = `{"route":"/","blocks":["hero","features"],"copy":["Ship it.","Built for developers."],"uses_asset":[]}`
	assetsReply  = `{"path":"public/hero.png","alt":"the terminal","kind":"screenshot"}`
	passReview   = `{"pass":0,"verdict":"pass","blocking":[],"from":"page","why":""}`
)

func baseReplies() map[Phase][]string {
	return map[Phase][]string{
		PhaseQualify: {qualifyReply},
		PhaseBrief:   {briefReply},
		PhaseBrand:   {brandReply},
		PhasePage:    {pageReply},
		PhaseAssets:  {assetsReply},
		PhaseBuild:   {`{"files":["index.html"],"notes":"done"}`},
		PhaseReview:  {passReview},
	}
}

func newRunner(t *testing.T, m *scriptedModel, dir string) *Runner {
	t.Helper()
	ag, _ := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-1", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunCarriesAPageFromRequestToReviewedVerdict(t *testing.T) {
	requireAgentBrowser(t)
	m := newScriptedModel(t, "site", baseReplies())
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-1", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}

	var seen []Progress
	res, err := r.Run(context.Background(), func(p Progress) { seen = append(seen, p) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Passed {
		t.Errorf("Passed = false, want true: %+v", res.Review)
	}
	if res.Repairs != 0 {
		t.Errorf("Repairs = %d, want 0", res.Repairs)
	}
	if len(res.Shots) != 2 {
		t.Fatalf("got %d shots, want 2", len(res.Shots))
	}

	pos, err := r.Store().Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseDone {
		t.Errorf("store position = %q, want %q", pos, PhaseDone)
	}

	// Every phase must be visited in order, and a run must not skip the gate.
	// Capture is reported from inside the preview step, so a progress view can
	// distinguish "serving" from "photographing".
	want := []Phase{PhaseQualify, PhaseBrief, PhaseBrand, PhasePage, PhaseAssets, PhaseBuild, PhasePreview, PhaseCapture, PhaseReview}
	var got []Phase
	for _, p := range seen {
		if len(got) == 0 || got[len(got)-1] != p.Phase {
			got = append(got, p.Phase)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("phases visited = %v, want %v", got, want)
	}

	// The page really was built, and really was photographed.
	if _, err := os.Stat(filepath.Join(dir, "site", "index.html")); err != nil {
		t.Errorf("site file missing: %v", err)
	}
	for _, shot := range res.Shots {
		data, err := os.ReadFile(shot.Path)
		if err != nil {
			t.Fatalf("shot %s: %v", shot.Path, err)
		}
		if len(data) == 0 {
			t.Errorf("shot %s is empty", shot.Path)
		}
	}
}

func TestRunStopsAtTheRepairBudgetAndSaysSo(t *testing.T) {
	requireAgentBrowser(t)
	replies := baseReplies()
	replies[PhasePage] = []string{pageReply, pageReply, pageReply, pageReply}
	replies[PhaseReview] = []string{
		`{"pass":0,"verdict":"repair","blocking":["hero is cramped"],"from":"page","why":"give the hero more room"}`,
		`{"pass":1,"verdict":"repair","blocking":["still cramped"],"from":"page","why":"more room"}`,
		`{"pass":2,"verdict":"repair","blocking":["still cramped"],"from":"page","why":"more room"}`,
	}
	m := newScriptedModel(t, "site", replies)
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-budget", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Passed {
		t.Error("a run that never passed reported success")
	}
	if res.Repairs != MaxRepairs {
		t.Errorf("Repairs = %d, want %d", res.Repairs, MaxRepairs)
	}
	if m.calls[PhaseReview] != MaxRepairs+1 {
		t.Errorf("review ran %d times, want %d: a spent budget must end the run",
			m.calls[PhaseReview], MaxRepairs+1)
	}
}

// A repair must rewind to the phase the reviewer named and no further, or the
// run redoes work that was already accepted.
func TestRunRewindsOnlyToTheNamedPhase(t *testing.T) {
	requireAgentBrowser(t)
	replies := baseReplies()
	replies[PhaseReview] = []string{
		`{"pass":0,"verdict":"repair","blocking":["palette is muddy"],"from":"brand","why":"raise contrast"}`,
		passReview,
	}
	m := newScriptedModel(t, "site", replies)
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-rewind", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.calls[PhaseQualify] != 1 {
		t.Errorf("qualify ran %d times, want 1: a brand repair must not redo qualification", m.calls[PhaseQualify])
	}
	if m.calls[PhaseBrief] != 1 {
		t.Errorf("brief ran %d times, want 1: a brand repair must not redo the brief", m.calls[PhaseBrief])
	}
	if m.calls[PhaseBrand] != 2 {
		t.Errorf("brand ran %d times, want 2", m.calls[PhaseBrand])
	}
	if m.calls[PhasePage] != 2 {
		t.Errorf("page ran %d times, want 2", m.calls[PhasePage])
	}
}

// A reply the store will not accept must be re-asked, a bounded number of times,
// and the run must fail rather than spin.
func TestRunGivesUpOnAnUnusableReply(t *testing.T) {
	m := newScriptedModel(t, "site", map[Phase][]string{
		PhaseQualify: {"I would rather not.", "still no.", "no."},
	})
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-bad", SiteDir: m.siteDir, Tool: ToolStatic, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("Run succeeded with no acceptable reply")
	}
	if m.calls[PhaseQualify] != 3 {
		t.Errorf("qualify asked %d times, want the configured 3", m.calls[PhaseQualify])
	}
	if m.calls[PhaseBrief] != 0 {
		t.Error("the run continued past a phase that produced nothing")
	}
}

// The brief is the hard gate: nothing may be built before it validates.
func TestRunWillNotBuildBeforeTheBriefIsAccepted(t *testing.T) {
	m := newScriptedModel(t, "site", map[Phase][]string{
		PhaseQualify: {qualifyReply},
		PhaseBrief:   {`{"summary":"","sections":[]}`},
	})
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-gate", SiteDir: m.siteDir, Tool: ToolStatic, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("Run built the page with an invalid brief")
	}
	if m.calls[PhaseBuild] != 0 {
		t.Error("the build phase ran before the brief validated")
	}
}

// A run killed mid-pipeline must resume from its artifacts rather than redoing
// accepted work, which is the whole reason position is derived.
func TestRunResumesFromWhereItStopped(t *testing.T) {
	m := newScriptedModel(t, "site", baseReplies())
	_, dir := newRunAgent(t, m)

	// Stand in for a run killed after the brand: the artifacts it had accepted
	// are on disk, and a fresh process picks them up.
	store, err := NewStore(dir, "run-resume")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []artifact{
		Qualify{Goal: "a page", Audience: "devs"},
		Brief{Summary: "one page", Sections: []string{"hero"}, Mood: "calm"},
		Brand{Name: "Escape", Palette: []string{"#0b0d10"}},
	} {
		if err := store.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	if pos, _ := store.Position(); pos != PhasePage {
		t.Fatalf("position = %q, want %q", pos, PhasePage)
	}

	ag2, _ := newRunAgent(t, m)
	r2, err := NewRunner(ag2, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-resume", SiteDir: "site", Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Run(context.Background(), nil); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if m.calls[PhaseQualify] != 0 || m.calls[PhaseBrief] != 0 || m.calls[PhaseBrand] != 0 {
		t.Errorf("resume redid accepted phases: qualify=%d brief=%d brand=%d",
			m.calls[PhaseQualify], m.calls[PhaseBrief], m.calls[PhaseBrand])
	}
	if m.calls[PhasePage] != 1 {
		t.Errorf("page ran %d times on resume, want 1", m.calls[PhasePage])
	}
}

func TestRunRefusesASiteDirectoryOutsideTheWorkspace(t *testing.T) {
	m := newScriptedModel(t, "site", baseReplies())
	ag, dir := newRunAgent(t, m)
	for _, site := range []string{"..", "/etc", "a/../../escape"} {
		if _, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-x", SiteDir: site, Tool: ToolStatic}); err == nil {
			t.Errorf("NewRunner accepted site directory %q", site)
		}
	}
}

func TestRunRefusesAnInvalidSessionID(t *testing.T) {
	m := newScriptedModel(t, "site", baseReplies())
	ag, dir := newRunAgent(t, m)
	if _, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "../escape", SiteDir: "site"}); err == nil {
		t.Error("NewRunner accepted a session id containing a separator")
	}
}

func TestReviewPromptNamesBothScreenshotsAndTheBudget(t *testing.T) {
	s, err := NewStore(t.TempDir(), "prompts")
	if err != nil {
		t.Fatal(err)
	}
	p := reviewPrompt(s, 0, nil)
	for _, want := range []string{shotFile(Desktop), shotFile(Mobile), "pass 0"} {
		if !strings.Contains(p, want) {
			t.Errorf("review prompt does not mention %q", want)
		}
	}
	if strings.Contains(p, fmt.Sprintf("of at most %d", MaxRepairs)) {
		t.Error("first review claims to be a repair pass")
	}
	if p2 := reviewPrompt(s, 1, nil); !strings.Contains(p2, fmt.Sprintf("of at most %d", MaxRepairs)) {
		t.Error("repair review does not state the budget")
	}
}

func TestEveryPhaseRuleExists(t *testing.T) {
	// A phase with no rule would send the model a prompt with no contract, and
	// the run would fail at the parse boundary with nothing to say why.
	for _, p := range []Phase{PhaseQualify, PhaseBrief, PhaseBrand, PhasePage, PhaseAssets, PhaseBuild, PhaseReview} {
		if strings.TrimSpace(phaseRules[p]) == "" {
			t.Errorf("phase %s has no rules", p)
		}
		if _, ok := newArtifact[p]; !ok {
			t.Errorf("phase %s has no artifact constructor", p)
		}
	}
	if _, ok := newArtifact[PhasePreview]; ok {
		t.Error("preview claims to produce an artifact")
	}
}

func TestPriorContextCarriesAcceptedArtifactsOnly(t *testing.T) {
	s, err := NewStore(t.TempDir(), "prior")
	if err != nil {
		t.Fatal(err)
	}
	if got := prior(s, PhasePage); got != "" {
		t.Errorf("prior with nothing accepted = %q, want empty", got)
	}
	if err := s.Save(Brief{Summary: "one page", Sections: []string{"hero"}}); err != nil {
		t.Fatal(err)
	}
	got := prior(s, PhasePage)
	if !strings.Contains(got, "one page") {
		t.Errorf("prior = %q, want the accepted brief", got)
	}
	if strings.Contains(got, "already agreed") && strings.Contains(got, "brand") {
		t.Error("prior leaked a phase that had not run yet")
	}
}

func TestParseArtifactOnEveryPhaseShape(t *testing.T) {
	// A round trip through the wire format, so a struct and its rule cannot
	// drift apart without one of these failing.
	cases := []struct {
		phase Phase
		reply string
	}{
		{PhaseQualify, qualifyReply},
		{PhaseBrief, briefReply},
		{PhaseBrand, brandReply},
		{PhasePage, pageReply},
		{PhaseAssets, assetsReply},
		{PhaseBuild, `{"files":["index.html"],"notes":"n"}`},
		{PhaseReview, passReview},
	}
	for _, c := range cases {
		art, err := parseArtifact(c.phase, c.reply)
		if err != nil {
			t.Errorf("parseArtifact(%s): %v", c.phase, err)
			continue
		}
		data, err := json.Marshal(art)
		if err != nil {
			t.Errorf("marshal %s: %v", c.phase, err)
			continue
		}
		if !strings.Contains(string(data), `"`) {
			t.Errorf("%s marshalled to nothing", c.phase)
		}
	}
}

// The repair budget exists to stop a run looping, so it cannot be the model's to
// grant. A model that answers "pass 0" to every verdict must still be cut off.
func TestRunIgnoresAModelThatUnderstatesItsOwnPass(t *testing.T) {
	requireAgentBrowser(t)
	replies := baseReplies()
	lies := `{"pass":0,"verdict":"repair","blocking":["nope"],"from":"page","why":"again"}`
	replies[PhaseReview] = []string{lies, lies, lies, lies, lies, lies, lies, lies}
	m := newScriptedModel(t, "site", replies)
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-liar", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Passed {
		t.Error("a run that never passed reported success")
	}
	if res.Repairs != MaxRepairs {
		t.Errorf("Repairs = %d, want %d", res.Repairs, MaxRepairs)
	}
	if got := m.calls[PhaseReview]; got != MaxRepairs+1 {
		t.Errorf("review ran %d times, want %d: the runner owns the budget, not the model",
			got, MaxRepairs+1)
	}
}

// The pass number on disk is the runner's count, so a resumed run and a fresh
// one agree on how far the run got.
func TestRunStampsThePassNumberItself(t *testing.T) {
	requireAgentBrowser(t)
	replies := baseReplies()
	replies[PhaseReview] = []string{
		`{"pass":0,"verdict":"repair","blocking":["x"],"from":"page","why":"y"}`,
		`{"pass":99,"verdict":"pass","blocking":[],"from":"page","why":""}`,
	}
	m := newScriptedModel(t, "site", replies)
	ag, dir := newRunAgent(t, m)
	r, err := NewRunner(ag, Config{Request: "a landing page for a Go RPC engine", Root: dir, SessionID: "run-stamp", SiteDir: m.siteDir, Tool: ToolStatic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	final, err := LoadReview(r.Store(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if final.Pass != 1 {
		t.Errorf("stored pass = %d, want the runner's count of 1, not the model's 99", final.Pass)
	}
}
