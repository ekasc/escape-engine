package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/design"
)

// The line repl's half of Design Mode.
//
// A repl has no event loop and no place to paint a phase list, so this one
// prints as it goes. It is deliberately not a thin wrapper around the TUI's
// command: the TUI appends styled lines to a viewport and stays interactive,
// while this blocks until the run ends. Sharing the run is right; sharing the
// presentation would mean one of them rendering in a medium it does not have.

func replDesign(ag *agent.Agent, stdout io.Writer, cwd, sessionID, request string) {
	svc := design.NewService()
	cfg := design.Config{
		Root:      cwd,
		SessionID: sessionID,
		SiteDir:   designSiteDir,
		Request:   request,
		Tool:      design.ToolStatic,
	}
	run, err := svc.Start(cwd, sessionID, designSiteDir, request, func() (*design.Runner, error) {
		return design.NewRunner(ag, cfg)
	})
	if err != nil {
		fmt.Fprintln(stdout, "design:", err)
		return
	}
	for u := range run.Updates() {
		if u.Progress != nil {
			fmt.Fprintln(stdout, designPhaseLine(*u.Progress))
			continue
		}
		if u.Err != nil && u.Result == nil {
			fmt.Fprintln(stdout, "design:", u.Err)
			return
		}
		if u.Result == nil {
			continue
		}
		fmt.Fprintln(stdout, designVerdictLine(*u.Result))
		for _, shot := range u.Result.Shots {
			fmt.Fprintf(stdout, "  %s  %s\n", shot.Viewport, shot.Path)
		}
		if u.Err != nil {
			fmt.Fprintln(stdout, "  "+u.Err.Error())
		}
	}
}

// sessionIDFor names a run's directory. The session file's base name is already
// a single safe path segment, which is what the store confines; falling back to
// a literal keeps a session-less repl usable rather than refusing to design.
func sessionIDFor(sessionPath string) string {
	base := sessionPath
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		return "repl"
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}
