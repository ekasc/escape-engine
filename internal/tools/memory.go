package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/ekasc/escape-engine/internal/memory"
)

// Memory lets the agent keep the few facts that must outlive this session:
// decisions, conventions, where something lives. It is bounded, so the agent
// has to choose, and a write past the cap fails rather than silently dropping
// entries.
func Memory(store *memory.Store) Tool {
	return Tool{
		Name:        "memory",
		Description: fmt.Sprintf("Manage durable notes for this project that survive session end. Bounded at %d chars. Entries are numbered from 1. Use op=add to record a decision or convention, replace to tighten one, remove to drop one, list to read them back. A write past the cap fails: consolidate first.", memory.MaxChars),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op": map[string]any{
					"type":        "string",
					"enum":        []string{"add", "replace", "remove", "list"},
					"description": "Operation to perform.",
				},
				"text":  map[string]any{"type": "string", "description": "Entry text. Required for add and replace."},
				"index": map[string]any{"type": "integer", "description": "1-based entry number. Required for replace and remove."},
			},
			"required": []string{"op"},
		},
		ParallelSafe: true,
		Run: func(_ context.Context, args map[string]any) Result {
			if store == nil {
				return Result{Output: "memory is unavailable for this session", IsError: true}
			}
			var p struct {
				Op    string `json:"op"`
				Text  string `json:"text"`
				Index int    `json:"index"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return Result{Output: "invalid memory arguments: " + err.Error(), IsError: true}
			}
			op, text, index := p.Op, p.Text, p.Index

			var err error
			switch op {
			case "add":
				err = store.Add(text)
			case "replace":
				err = store.Replace(index, text)
			case "remove":
				err = store.Remove(index)
			case "list":
			default:
				return Result{Output: "op must be one of: add, replace, remove, list", IsError: true}
			}
			if err != nil {
				return Result{Output: err.Error(), IsError: true}
			}
			return Result{Output: memoryReport(store)}
		},
	}
}

func memoryReport(store *memory.Store) string {
	entries, err := store.Entries()
	if err != nil {
		return "memory: " + err.Error()
	}
	used, _ := store.Used()
	if len(entries) == 0 {
		return fmt.Sprintf("memory is empty (0/%d chars)", memory.MaxChars)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "memory (%d/%d chars)\n", used, memory.MaxChars)
	for i, entry := range entries {
		fmt.Fprintf(&b, "%d. %s\n", i+1, entry)
	}
	return strings.TrimRight(b.String(), "\n")
}
