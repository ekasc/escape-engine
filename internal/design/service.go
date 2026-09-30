package design

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Service owns at most one design run per session and knows nothing about how a
// caller reaches it.
//
// The run is the same for every caller: it drives the session's own agent, so a
// design run shows up in the thread like any other work and there is no second
// transcript. What differs is the front door and the presentation — a terminal
// command prints progress as lines, the composer subscribes to it and renders
// its own. Neither is implemented by delegating to the other, because a caller
// that renders its own progress needs the events, and one that prints needs
// them synchronously. This type is the seam: it runs the run and reports
// progress to whoever asked.
type Service struct {
	mu      sync.Mutex
	byKey   map[string]*Run
	started bool
}

// Run is one design run in progress.
type Run struct {
	// Key identifies the run's owner, normally a session id. It names the run's
	// directory under the store, so it is the same value the store confines.
	Key string

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	progress []Progress
	result   Result
	err      error
}

// ErrBusy reports that a run is already in progress for this key.
var ErrBusy = errors.New("a design run is already in progress")

// NewService returns a service with no runs.
func NewService() *Service {
	return &Service{byKey: map[string]*Run{}}
}

// Start begins a run for key. It returns as soon as the run is under way, so a
// caller that wants to watch it can subscribe to Updates before the first phase
// reports; the run itself continues in the background.
func (s *Service) Start(root, key, siteDir, request string, build func() (*Runner, error)) (*Run, error) {
	if key == "" {
		return nil, errors.New("design run needs a session key")
	}
	if request == "" {
		return nil, errors.New("design run needs a request")
	}
	runner, err := build()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{Key: key, cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	if s.byKey[key] != nil {
		s.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("%s: %w", key, ErrBusy)
	}
	s.byKey[key] = run
	s.mu.Unlock()

	go func() {
		defer close(run.done)
		defer cancel()
		defer s.clear(key, run)
		// Every phase reports, including the ones that fail. A run that dies at
		// capture still told the caller which phase it reached, and swallowing
		// that leaves "nothing happened" as the only honest-sounding summary.
		result, err := runner.Run(ctx, func(p Progress) { run.record(p) })
		run.finish(result, err)
	}()
	return run, nil
}

func (s *Service) clear(key string, run *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey[key] == run {
		delete(s.byKey, key)
	}
}

// Get returns the run in progress for key, or nil.
func (s *Service) Get(key string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey[key]
}

// Busy reports whether key already has a run.
func (s *Service) Busy(key string) bool { return s.Get(key) != nil }

func (r *Run) record(p Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, p)
}

func (r *Run) finish(result Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result = result
	r.err = err
}

// Progress returns the phase reports so far. Safe to call while the run is
// going: a caller polls it, or drains it through Updates.
func (r *Run) Progress() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Progress(nil), r.progress...)
}

// Result returns the finished result and error. It is only meaningful once
// Done has closed.
func (r *Run) Result() (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result, r.err
}

// Done closes when the run has finished, one way or the other.
func (r *Run) Done() <-chan struct{} { return r.done }

// Stop cancels the run and waits for it to unwind, so a caller that returns from
// Stop knows the preview server is already down. It is safe to call more than
// once and on a run that has already finished.
func (r *Run) Stop() {
	r.cancel()
	<-r.done
}

// Update is one report from a run, for a caller that would rather be told than
// poll. Exactly one of Progress and Result is set; the final update carries the
// result.
type Update struct {
	Progress *Progress
	Result   *Result
	Err      error
}

// Updates delivers a run's reports in order and closes when the run ends.
//
// It replays what has already been reported before following along, so a caller
// that subscribes late does not miss the phases it slept through. The drain
// after the run closes is not redundant: a report can be appended between one
// snapshot and the run finishing, and a caller must not lose the phase that
// explains how the run ended.
func (r *Run) Updates() <-chan Update {
	out := make(chan Update, 64)
	go func() {
		defer close(out)
		seen := 0
		drain := func() {
			r.mu.Lock()
			pending := r.progress[seen:]
			seen = len(r.progress)
			r.mu.Unlock()
			for _, p := range pending {
				p := p
				out <- Update{Progress: &p}
			}
		}
		for {
			drain()
			select {
			case <-r.done:
				drain()
				result, err := r.Result()
				out <- Update{Result: &result, Err: err}
				return
			case <-time.After(progressPoll):
			}
		}
	}()
	return out
}

// progressPoll is how often Updates looks for a new phase report. It is short
// because a phase boundary is something a person is watching, not a batch
// boundary, and nothing here depends on catching one promptly.
const progressPoll = 100 * time.Millisecond
