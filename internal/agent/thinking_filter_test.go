package agent

import "testing"

func TestThinkingFilterRemovesSplitThinkingTags(t *testing.T) {
	filter := &thinkingFilter{}
	var visible string
	for _, chunk := range []string{"Hello <thi", "nk>private reasoning</thi", "nk> **world**"} {
		visible += filter.Feed(chunk)
	}
	visible += filter.Flush()
	if visible != "Hello  **world**" {
		t.Fatalf("visible text = %q", visible)
	}
}

func TestThinkingFilterDiscardsUnclosedThinking(t *testing.T) {
	filter := &thinkingFilter{}
	if got := filter.Feed("before <think>private"); got != "before " {
		t.Fatalf("visible before close = %q", got)
	}
	if got := filter.Flush(); got != "" {
		t.Fatalf("unclosed thinking leaked %q", got)
	}
}
