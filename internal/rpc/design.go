package rpc

import (
	"fmt"

	"github.com/ekasc/escape-engine/internal/design"
)

// The composer's half of Design Mode.
//
// This is a dedicated surface, not a command the shell asks the engine to run
// and then reads the output of. The reason is what the composer has to show: a
// phase list, a repair counter, a review verdict and two screenshots, rendered
// into the thread while the run is going. A caller that had to parse terminal
// output to get that would be reconstructing a state machine it could have been
// handed, and it would break the moment the terminal formatting changed.
//
// The engine's own /design command shares the run and the artifacts, because
// that is the domain. It shares nothing here.

// designSiteDir is where a composer's run writes its page. It matches the
// terminal's so a run started from either surface lands in the same place.
const designSiteDir = "site"

// designSessionKey names a run's directory. The session id is already a single
// safe path segment, which is what the store confines.
func (s *PiServer) designSessionKey() string {
	if s.store != nil {
		if id := s.store.ID(); id != "" {
			return id
		}
	}
	return "composer"
}

// designActive reports whether a design run holds this session. It gates prompt
// handling too: a design run drives the same agent as an ordinary turn, so
// letting the user send while it works would be two turns on one agent.
func (s *PiServer) designActive() bool {
	return s.design != nil && s.design.Busy(s.designSessionKey())
}

// startDesign begins a run for the composer and relays its progress as events.
//
// It answers immediately. A design run is several model calls and a page
// capture, and the shell's send is a request/response pair with no way to hold a
// response open for that without teaching the shell to wait, which is what the
// event stream is for.
func (s *PiServer) startDesign(req piRequest, cmd string) {
	if s.designActive() {
		s.finish(req, cmd, nil, fmt.Errorf("a design run is already in progress"))
		return
	}
	request := req.DesignRequest
	if request == "" {
		s.finish(req, cmd, nil, fmt.Errorf("design needs a request"))
		return
	}
	s.mu.Lock()
	ag := s.agent
	s.mu.Unlock()
	if ag == nil {
		s.finish(req, cmd, nil, fmt.Errorf("no session is open"))
		return
	}

	key := s.designSessionKey()
	siteDir := req.DesignSiteDir
	if siteDir == "" {
		siteDir = designSiteDir
	}
	cfg := design.Config{
		Root:      s.cwd,
		SessionID: key,
		SiteDir:   siteDir,
		Request:   request,
		Tool:      design.ToolStatic,
	}
	run, err := s.design.Start(s.cwd, key, siteDir, request, func() (*design.Runner, error) {
		return design.NewRunner(ag, cfg)
	})
	if err != nil {
		s.finish(req, cmd, nil, err)
		return
	}
	s.finish(req, cmd, map[string]any{"started": true}, nil)

	// Progress is an event stream rather than a second request, because phases
	// arrive over minutes and the shell wants each one as it lands.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for u := range run.Updates() {
			if u.Progress != nil {
				s.emit(map[string]any{
					"type":   "design_progress",
					"phase":  string(u.Progress.Phase),
					"pass":   u.Progress.Pass,
					"detail": u.Progress.Detail,
				})
				continue
			}
			payload := map[string]any{"type": "design_done"}
			if u.Result != nil {
				payload["passed"] = u.Result.Passed
				payload["repairs"] = u.Result.Repairs
				payload["verdict"] = string(u.Result.Review.Verdict)
				payload["why"] = u.Result.Review.Why
				shots := make([]any, 0, len(u.Result.Shots))
				for _, shot := range u.Result.Shots {
					shots = append(shots, map[string]any{
						"viewport": shot.Viewport.String(),
						"path":     shot.Path,
					})
				}
				payload["shots"] = shots
			}
			if u.Err != nil {
				payload["error"] = u.Err.Error()
			}
			s.emit(payload)
		}
	}()
}

// cancelDesign stops a run and waits for the preview server to go down, so the
// caller knows the port is free when the response arrives.
func (s *PiServer) cancelDesign(req piRequest, cmd string) {
	if s.design == nil {
		s.finish(req, cmd, map[string]any{"stopped": false}, nil)
		return
	}
	run := s.design.Get(s.designSessionKey())
	if run == nil {
		s.finish(req, cmd, map[string]any{"stopped": false}, nil)
		return
	}
	run.Stop()
	s.finish(req, cmd, map[string]any{"stopped": true}, nil)
}
