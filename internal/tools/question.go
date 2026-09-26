package tools

import "context"

func Question() Tool {
	return Tool{
		Name:        "question",
		Description: "Ask the user a clarification question when a decision or missing detail blocks progress.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question to ask the user"},
				"choices":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional answer choices"},
			},
			"required": []string{"question"},
		},
		Run: func(context.Context, map[string]any) Result {
			return Result{Output: "question must be answered through the agent event channel", IsError: true}
		},
	}
}
