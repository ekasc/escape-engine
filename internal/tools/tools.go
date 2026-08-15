// Package tools implements the v1 tool set: bash, file read/write, patch
// edit, grep, glob.
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/ekasc/pi-go/internal/provider"
)

// Result is what a tool returns to the agent loop.
type Result struct {
	Output  string
	IsError bool
}

// Tool is a callable the model can invoke.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON schema
	Run         func(ctx context.Context, args map[string]any) Result
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
func Default(cwd string) []Tool {
	return []Tool{
		Bash(cwd),
		Read(cwd),
		Write(cwd),
		Edit(cwd),
		Grep(cwd),
		Glob(cwd),
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
