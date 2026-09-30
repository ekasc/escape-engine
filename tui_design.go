package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ekasc/escape-engine/internal/design"
)

// The terminal's half of Design Mode.
//
// This is a front door, not a client of the other one. It starts the run
// through the same design.Service the RPC layer uses, because the phase machine
// and its artifacts are the domain; it renders its own progress as transcript
// lines and prints its own verdict, because a person in a terminal and a person
// looking at the composer are not reading the same thing. The composer never
// reads these lines, and nothing here reads the composer's events.

// designSiteDir is where a design run writes its page, relative to the
// workspace. It is a constant rather than an argument so the terminal and the
// composer cannot disagree about where a run left its files.
const designSiteDir = "site"

// startDesign begins a design run and returns immediately. The run reports from
// its own goroutine and posts each phase into the event loop, so the UI stays
// responsive while the model works.
func (m *tuiModel) startDesign(request string) tea.Cmd {
	if m.busy {
		m.appendLine(tuiErrorStyle.Render("stop the current turn before starting a design run"), true)
		return nil
	}
	request = strings.TrimSpace(request)
	if request == "" {
		m.appendLine(tuiErrorStyle.Render("usage: /design <what to build>"), true)
		return nil
	}
	if m.design == nil {
		m.appendLine(tuiErrorStyle.Render("design is unavailable in this build"), true)
		return nil
	}
	if m.design.Busy(m.designKey()) {
		m.appendLine(tuiErrorStyle.Render("a design run is already in progress"), true)
		return nil
	}
	run, err := m.newDesignRun(request)
	if err != nil {
		m.appendLine(tuiErrorStyle.Render("design: "+err.Error()), true)
		return nil
	}
	m.setBusy(true)
	m.status = "designing"
	go m.pumpDesign(run)
	return nil
}

// newDesignRun constructs the run against this session's agent. The same agent
// and session the terminal already uses, so a design run's turns land in the
// transcript like any other work.
func (m *tuiModel) newDesignRun(request string) (*design.Run, error) {
	key := m.designKey()
	cfg := design.Config{
		Root:      m.cwd,
		SessionID: key,
		SiteDir:   designSiteDir,
		Request:   request,
		Tool:      design.ToolStatic,
	}
	return m.design.Start(m.cwd, key, designSiteDir, request, func() (*design.Runner, error) {
		return design.NewRunner(m.agent, cfg)
	})
}

// pumpDesign forwards a run's reports into the event loop. Posting is the only
// thread-safe way in, and doing it here rather than from the run keeps the
// design package free of any UI.
func (m *tuiModel) pumpDesign(run *design.Run) {
	if m.post == nil {
		return
	}
	for u := range run.Updates() {
		if u.Progress != nil {
			m.post(tuiDesignMsg{progress: u.Progress})
			continue
		}
		m.post(tuiDesignMsg{result: u.Result, err: u.Err})
	}
}

// designKey names the run's directory under the store. The session id is
// already a single safe path segment, which is what the store requires.
func (m *tuiModel) designKey() string {
	if m.store != nil {
		if id := m.store.ID(); id != "" {
			return id
		}
	}
	return "tui"
}

// updateDesign renders one report from a run.
func (m *tuiModel) updateDesign(msg tuiDesignMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil && msg.result == nil {
		m.setBusy(false)
		m.status = "error"
		m.appendLine(tuiErrorStyle.Render("design: "+msg.err.Error()), true)
		return m, nil
	}
	if msg.progress != nil {
		m.appendLine(designPhaseLine(*msg.progress), true)
		return m, nil
	}
	if msg.result != nil {
		m.setBusy(false)
		m.status = "idle"
		m.appendLine("", false)
		m.appendLine(designVerdictLine(*msg.result), true)
		for _, shot := range msg.result.Shots {
			m.appendLine(tuiMutedStyle.Render(fmt.Sprintf("  %s  %s", shot.Viewport, shot.Path)), true)
		}
		if msg.err != nil {
			m.appendLine(tuiErrorStyle.Render("  "+msg.err.Error()), true)
		}
	}
	return m, nil
}

// designPhaseLine is one phase's report. A phase that reported a detail is
// showing why it was rejected or what it decided, so the detail is the line's
// content rather than an afterthought.
func designPhaseLine(p design.Progress) string {
	label := fmt.Sprintf("design · %s", p.Phase)
	if p.Pass > 0 {
		label += fmt.Sprintf(" (repair %d/%d)", p.Pass, design.MaxRepairs)
	}
	if p.Detail == "" {
		return tuiToolStyle.Render(label)
	}
	return tuiToolStyle.Render(label) + tuiMutedStyle.Render("  "+p.Detail)
}

// designVerdictLine states the outcome in the one sentence that matters. A run
// that ran out of repair budget did not fail; it stopped being able to improve,
// and saying so is more useful than calling it a failure.
func designVerdictLine(r design.Result) string {
	switch {
	case r.Passed:
		return tuiUserStyle.Render("design · passed review")
	case r.Review.Why != "":
		return tuiErrorStyle.Render("design · not accepted after " +
			fmt.Sprintf("%d repair(s): %s", r.Repairs, r.Review.Why))
	default:
		return tuiErrorStyle.Render(fmt.Sprintf("design · not accepted after %d repair(s)", r.Repairs))
	}
}
