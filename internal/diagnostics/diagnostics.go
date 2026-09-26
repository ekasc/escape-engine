// Package diagnostics records what a turn actually cost, so the answer to "why
// was that slow" is a number instead of a guess.
//
// The phases exist because they fail differently. Reading history is disk and
// grows with the transcript. Building the request is CPU and grows with the
// conversation. Waiting for the provider is the network and the model. Only
// measuring the total tells you which one to go and fix.
package diagnostics

import (
	"sort"
	"sync"
	"time"
)

// Turn is one completed agent turn and what it cost.
type Turn struct {
	At         time.Time `json:"at"`
	Iterations int       `json:"iterations"`
	// Durations are milliseconds rather than time.Duration because
	// time.Duration marshals to nanoseconds, and a renderer showing
	// "2026000000ms" is worse than no number at all.
	// HistoryReadMs is reading the session file for this iteration.
	HistoryReadMs int64 `json:"historyReadMs"`
	// BuildMs is turning entries and tool specs into a provider request.
	BuildMs int64 `json:"buildMs"`
	// FirstTokenMs is from issuing the provider request to its first event.
	FirstTokenMs int64 `json:"firstTokenMs"`
	// TotalMs is the whole turn, from the message arriving to the agent settling.
	TotalMs int64 `json:"totalMs"`
	// PromptTokens and ContextTokens come from provider usage when reported.
	PromptTokens  int `json:"promptTokens,omitempty"`
	ContextTokens int `json:"contextTokens,omitempty"`
	// ContextWindow is the window Escape believes the model has, which is an
	// assumption for any model outside its small table.
	ContextWindow int `json:"contextWindow"`
	// Compacted records that auto-compaction ran during this turn.
	Compacted bool `json:"compacted,omitempty"`
	// Error is the failure text when the turn ended badly.
	Error string `json:"error,omitempty"`
}

// Snapshot is the state a diagnostics read returns.
type Snapshot struct {
	Version        string        `json:"version,omitempty"`
	Provider       string        `json:"provider,omitempty"`
	Model          string        `json:"model,omitempty"`
	Thinking       string        `json:"thinkingLevel,omitempty"`
	SessionID      string        `json:"sessionId,omitempty"`
	SessionPath    string        `json:"sessionPath,omitempty"`
	SessionSize    int64         `json:"sessionSizeBytes,omitempty"`
	SessionEntries int           `json:"sessionEntries,omitempty"`
	StartedAt      time.Time     `json:"startedAt,omitempty"`
	Uptime         time.Duration `json:"uptime,omitempty"`
	UptimeMs       int64         `json:"uptimeMs"`
	Skills         int           `json:"skills,omitempty"`
	Tools          int           `json:"tools,omitempty"`
	ContextWindow  int           `json:"contextWindow,omitempty"`
	LastError      string        `json:"lastError,omitempty"`
	Turns          []Turn        `json:"turns,omitempty"`
	// FirstTokenP50 and P95 across recorded turns, which is the number worth
	// watching. A single slow turn is noise; a p95 that moved is a regression.
	FirstTokenP50 int64 `json:"firstTokenP50Ms"`
	FirstTokenP95 int64 `json:"firstTokenP95Ms"`
	TotalP50      int64 `json:"totalP50Ms"`
}

// Recorder accumulates turns for one engine process.
type Recorder struct {
	mu        sync.Mutex
	startedAt time.Time
	turns     []Turn
	// keep bounds memory. Turn timings are a rolling window, not a log.
	keep int
}

const defaultKeep = 50

// NewRecorder returns a recorder that retains the most recent turns.
func NewRecorder() *Recorder {
	return &Recorder{startedAt: time.Now(), keep: defaultKeep}
}

// Record files one completed turn.
func (r *Recorder) Record(t Turn) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.At.IsZero() {
		t.At = time.Now()
	}
	r.turns = append(r.turns, t)
	if len(r.turns) > r.keep {
		r.turns = r.turns[len(r.turns)-r.keep:]
	}
}

// Last returns the most recent recorded turn.
func (r *Recorder) Last() (Turn, bool) {
	if r == nil {
		return Turn{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return Turn{}, false
	}
	return r.turns[len(r.turns)-1], true
}

// Snapshot returns the accumulated state. The caller fills in the descriptive
// fields it alone knows, such as provider and session path.
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		StartedAt: r.startedAt,
		Uptime:    time.Since(r.startedAt),
		UptimeMs:  time.Since(r.startedAt).Milliseconds(),
		Turns:     append([]Turn(nil), r.turns...),
	}
	firsts := make([]int64, 0, len(r.turns))
	totals := make([]int64, 0, len(r.turns))
	for _, t := range r.turns {
		firsts = append(firsts, t.FirstTokenMs)
		totals = append(totals, t.TotalMs)
		if t.Error != "" {
			s.LastError = t.Error
		}
		if t.ContextWindow > 0 {
			s.ContextWindow = t.ContextWindow
		}
	}
	s.FirstTokenP50 = percentile(firsts, 0.50)
	s.FirstTokenP95 = percentile(firsts, 0.95)
	s.TotalP50 = percentile(totals, 0.50)
	return s
}

func percentile(values []int64, q float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * q)
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}
