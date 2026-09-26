// Package tools implements the v1 tool set: bash, file read/write, patch
// edit, grep, glob, web search, clarification, and durable project memory.
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/ekasc/escape/engine/internal/memory"
	"github.com/ekasc/escape/engine/internal/provider"
	"github.com/ekasc/escape/engine/internal/session"
)

// Result is what a tool returns to the agent loop.
type Result struct {
	Output  string
	IsError bool
}

// Tool is a callable the model can invoke.
type Tool struct {
	Name         string
	Description  string
	Parameters   map[string]any // JSON schema
	ParallelSafe bool           `json:"parallelSafe,omitempty"`
	Run          func(ctx context.Context, args map[string]any) Result
}

// Registry is a concurrency-safe tool lookup.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// New builds a registry from tools, keyed by name.
func New(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.tools[t.Name] = t
	}
	return r
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns all tools, sorted by name.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Specs returns provider tool declarations for the registry.
func (r *Registry) Specs() []provider.ToolSpec {
	ts := r.List()
	specs := make([]provider.ToolSpec, 0, len(ts))
	for _, t := range ts {
		specs = append(specs, provider.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return specs
}

// Default returns the v1 tool set for a workspace.
// Deps are the engine-level collaborators a tool set needs. They are passed as
// a struct because the set grows: a positional list would be four unrelated
// arguments by the third one.
type Deps struct {
	// Cwd is the working directory for filesystem and shell tools.
	Cwd string
	// Memory is this project's durable store. Nil disables the memory tool
	// rather than failing the session.
	Memory *memory.Store
	// SessionRoot and Cwd together scope session search to one project.
	SessionRoot string
	// CurrentSession reports the live session path. It is a function because
	// the session changes underneath a long-lived tool set.
	CurrentSession func() string
	// Control carries live session state between the tool set and the server.
	Control *session.Control
}

// Default is the built-in tool set.
func Default(d Deps) []Tool {
	current := d.CurrentSession
	if current == nil {
		current = func() string { return "" }
	}
	return []Tool{
		Bash(d.Cwd),
		Read(d.Cwd),
		Write(d.Cwd),
		Edit(d.Cwd),
		Question(),
		WebSearch(),
		Grep(d.Cwd),
		Glob(d.Cwd),
		Memory(d.Memory),
		Sessions(d.SessionRoot, d.Cwd, current, d.Control),
	}
}

// errResult wraps a plain error as a failed tool result.
func errResult(err error) Result {
	return Result{Output: err.Error(), IsError: true}
}

// decodeArgs decodes the model-supplied arguments map into a struct.
func decodeArgs(args map[string]any, out any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
