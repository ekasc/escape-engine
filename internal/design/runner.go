package design

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ekasc/escape-engine/internal/agent"
	"github.com/ekasc/escape-engine/internal/session"
)

// maxArtifactAttempts bounds how many times one phase may be re-asked for a
// reply the store will accept. It is separate from the repair budget: a repair
// is a judgement that the design is wrong, whereas this is a retry because a
// reply did not parse. Without a bound here a model that keeps answering in
// prose runs forever.
const maxArtifactAttempts = 3

// Config configures a run.
type Config struct {
	// Root is the workspace. Every path a run touches is confined to it.
	Root string
	// SessionID names the run's directory. It must be a single path segment.
	SessionID string
	// SiteDir is where the page is built, relative to Root.
	SiteDir string
	// Request is what the user asked for, in their words. It is the input the
	// qualify phase reads, so a run without one has nothing to decide about.
	Request string
	// Tool is the allowlisted preview server.
	Tool Tool
	// MaxAttempts overrides maxArtifactAttempts when positive.
	MaxAttempts int
}

func (c Config) attempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return maxArtifactAttempts
}

// Progress reports where a run got to. A caller renders it; the runner does not
// decide how. The design package has no opinion about the shell.
type Progress struct {
	Phase  Phase
	Pass   int
	Detail string
}

// Result is a finished run.
type Result struct {
	// Passed reports whether the final review accepted the page. A run that
	// exhausts its repair budget finishes with Passed false and no error: the
	// design is not good enough, which is a result, not a malfunction.
	Passed bool
	// Review is the last verdict the run reached.
	Review Review
	// Repairs is how many repair passes were spent.
	Repairs int
	// Shots are the final screenshots, when the run got as far as capture.
	Shots []Shot
}

// Runner drives a design run through its phases.
//
// It drives the same agent a normal turn uses, with the same tools, approvals
// and session. There is no second agent and no second transcript: a design run
// is a sequence of ordinary turns, and it shows up in the thread like any other
// work.
type Runner struct {
	cfg     Config
	agent   *agent.Agent
	store   *Store
	shots   []Shot
	offset  int64
	repairs int
}

// NewRunner prepares a run. It does not start anything, so a caller can discover
// an invalid configuration before spending a turn.
func NewRunner(ag *agent.Agent, cfg Config) (*Runner, error) {
	if cfg.SiteDir == "" {
		cfg.SiteDir = "site"
	}
	if cfg.Tool == "" {
		cfg.Tool = ToolStatic
	}
	if strings.TrimSpace(cfg.Request) == "" {
		// Refusing here is the only place the omission can be caught cheaply.
		// A run started without one gets as far as asking a model whether the
		// request is a design task while never telling it what the request is.
		return nil, errors.New("design run needs a request")
	}
	store, err := NewStore(cfg.Root, cfg.SessionID)
	if err != nil {
		return nil, err
	}
	if _, err := confine(cfg.Root, cfg.SiteDir); err != nil {
		return nil, fmt.Errorf("site directory: %w", err)
	}
	// The repair count belongs to the run, not to this process. A run resumed
	// after a crash still carries the reviews it has already written, and
	// starting at zero would grant a fresh budget and overwrite the early
	// passes. The count to resume with is the number of reviews on disk, which
	// is also the number the next verdict has to be numbered — Repairs() is one
	// lower, because it counts finished repairs rather than passes begun.
	passes, err := store.Reviews()
	if err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, agent: ag, store: store, repairs: len(passes)}, nil
}

// Store is the run's artifact store. A caller resuming a run reads Position from
// the same place Run does.
func (r *Runner) Store() *Store { return r.store }

// Run drives the run to completion or to an exhausted repair budget.
//
// It resumes rather than restarts: the position comes from the artifacts on
// disk, so calling Run again after a crash picks up where the artifacts stop.
func (r *Runner) Run(ctx context.Context, report func(Progress)) (Result, error) {
	if report == nil {
		report = func(Progress) {}
	}
	for {
		pos, err := r.store.Position()
		if err != nil {
			return Result{}, err
		}
		if pos == PhaseDone {
			return r.finish()
		}
		report(Progress{Phase: pos, Pass: r.repairs})

		stop, err := r.step(ctx, pos, report)
		if err != nil {
			return Result{}, err
		}
		if stop {
			return r.finish()
		}
	}
}

// step performs one phase and reports whether the run is over.
//
// The stop flag exists because a review that asks for a repair the budget cannot
// buy is a finished run, not a phase to retry. Deriving position from artifacts
// cannot express that on its own: the last review on disk still says "repair",
// so position keeps reporting review, and a runner that treated "budget spent"
// as success would re-enter review forever.
func (r *Runner) step(ctx context.Context, pos Phase, report func(Progress)) (bool, error) {
	switch pos {
	case PhasePreview, PhaseCapture:
		return false, r.serveAndCapture(ctx, report)
	case PhaseBuild:
		return false, r.askFor(ctx, PhaseBuild, report)
	case PhaseReview:
		return r.review(ctx, report)
	default:
		return false, r.askFor(ctx, pos, report)
	}
}

// askFor runs one artifact-producing phase, re-asking a bounded number of times
// when the reply is not an artifact the store will accept.
func (r *Runner) askFor(ctx context.Context, pos Phase, report func(Progress)) error {
	var last error
	for attempt := 0; attempt < r.cfg.attempts(); attempt++ {
		prompt := phasePrompt(r.store, pos, r.cfg.SiteDir, r.cfg.Request, last)
		reply, err := r.turn(ctx, prompt)
		if err != nil {
			return err
		}
		art, err := parseArtifact(pos, reply)
		if err == nil {
			err = r.store.Save(art)
		}
		if err == nil {
			report(Progress{Phase: pos, Pass: r.repairs, Detail: "accepted"})
			return nil
		}
		last = err
		report(Progress{Phase: pos, Pass: r.repairs, Detail: err.Error()})
	}
	return fmt.Errorf("%s: no acceptable reply after %d attempts: %w", pos, r.cfg.attempts(), last)
}

// serveAndCapture starts the preview server, photographs the page and stops the
// server. The server is stopped on every path out of here, including a failed
// capture, because a leaked dev server holds its port for the rest of the run.
func (r *Runner) serveAndCapture(ctx context.Context, report func(Progress)) error {
	pv, err := StartPreview(ctx, r.cfg.Root, r.cfg.SiteDir, r.cfg.Tool)
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	defer pv.Stop(context.WithoutCancel(ctx))

	cap, err := NewCapturer(r.cfg.SessionID)
	if err != nil {
		return err
	}
	shots, err := cap.Capture(ctx, pv.URL(), r.store.Dir())
	if err != nil {
		if BrowserUnavailable(err) {
			// A machine that cannot start the browser is not a broken page, and
			// saying "exit status 1" sends the reader looking in the wrong place.
			return fmt.Errorf("capture: %w; the page was served but could not be photographed", err)
		}
		return fmt.Errorf("capture: %w", err)
	}
	r.shots = shots
	report(Progress{Phase: PhaseCapture, Pass: r.repairs, Detail: "captured"})
	return nil
}

// review asks for a verdict and applies it. A verdict of repair rewinds the run
// to the phase the reviewer named, or stops the run when the budget is spent.
func (r *Runner) review(ctx context.Context, report func(Progress)) (bool, error) {
	var last error
	for attempt := 0; attempt < r.cfg.attempts(); attempt++ {
		reply, err := r.turn(ctx, reviewPrompt(r.store, r.repairs, last))
		if err != nil {
			return false, err
		}
		rev, err := parseReview(reply)
		if err == nil {
			// The pass number is the runner's own count, never the model's. A
			// model that answers "pass 0" to every verdict would otherwise be
			// granted an unbounded number of repairs, which is the one thing the
			// budget exists to prevent.
			rev.Pass = r.repairs
			err = r.store.Save(rev)
		}
		if err != nil {
			last = err
			report(Progress{Phase: PhaseReview, Pass: r.repairs, Detail: err.Error()})
			continue
		}

		if !rev.NeedsRepair() {
			report(Progress{Phase: PhaseReview, Pass: rev.Pass, Detail: "passed"})
			return true, nil
		}
		if r.repairs >= MaxRepairs {
			report(Progress{Phase: PhaseReview, Pass: rev.Pass, Detail: "repair budget spent"})
			return true, nil
		}
		if err := r.store.Drop(rev.From); err != nil {
			return false, err
		}
		r.repairs++
		report(Progress{Phase: rev.From, Pass: r.repairs, Detail: "repair: " + rev.Why})
		return false, nil
	}
	return false, fmt.Errorf("review: no acceptable verdict after %d attempts: %w", r.cfg.attempts(), last)
}

// finish assembles the result of a run whose position is done.
func (r *Runner) finish() (Result, error) {
	passes, err := r.store.Reviews()
	if err != nil {
		return Result{}, err
	}
	if len(passes) == 0 {
		return Result{}, fmt.Errorf("run is done but no review was written")
	}
	rev, err := LoadReview(r.store, passes[len(passes)-1])
	if err != nil {
		return Result{}, err
	}
	repairs, err := r.store.Repairs()
	if err != nil {
		return Result{}, err
	}
	return Result{Passed: !rev.NeedsRepair(), Review: rev, Repairs: repairs, Shots: r.shots}, nil
}

// turn runs one agent turn and returns the assistant's reply.
//
// The reply is read back from the session rather than accumulated from the event
// bus, because the bus drops events for a subscriber that falls behind and a
// design reply can be long. The transcript is also where the answer has to come
// from: the user sees the same text either way.
func (r *Runner) turn(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	events := r.agent.Events()
	defer r.agent.Unsubscribe(events)

	if _, err := r.agent.Send(prompt, nil); err != nil {
		return "", err
	}
	for ev := range events {
		if ev.Event != agent.EventSettled {
			continue
		}
		if ev.Reason == agent.ReasonError {
			return "", errors.New(ev.Reason + ": " + ev.Message)
		}
		return r.lastAssistantText()
	}
	return "", errors.New("turn ended without settling")
}

// lastAssistantText returns the newest assistant message appended since the last
// call. Reading only the new tail keeps a turn on a long session proportional
// to the turn rather than to the transcript.
func (r *Runner) lastAssistantText() (string, error) {
	path := r.agent.SessionPath()
	for attempt := 0; attempt < 3; attempt++ {
		entries, offset, err := session.Since(path, r.offset, 1<<20)
		if err != nil {
			return "", err
		}
		r.offset = offset
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Type == session.TypeMessage && entries[i].Message != nil &&
				entries[i].Message.Role == session.RoleAssistant {
				return entries[i].Message.Text(false), nil
			}
		}
	}
	return "", errors.New("assistant reply not found in the session")
}
