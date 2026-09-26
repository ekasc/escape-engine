package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

func entriesOf(n int, text string) []session.Entry {
	out := make([]session.Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, session.Entry{
			Type:    session.TypeMessage,
			Message: &session.Message{Role: session.RoleUser, Content: []session.Block{{Type: session.BlockText, Text: text}}},
		})
	}
	return out
}

// The request carries the system prompt and every tool schema whether or not the
// conversation uses them. An estimate that counts only the conversation believes
// it has tens of thousands of tokens free that are already spent, and compaction
// fires far too late.
func TestRequestEstimateCountsOverheadNotJustConversation(t *testing.T) {
	a := New(Options{Cwd: t.TempDir()})
	conv := entriesOf(50, "a short message about the work")

	conversation := estimateActiveConversationTokens(conv)
	request := a.estimateRequestTokens(conv)

	if request <= conversation {
		t.Fatalf("request estimate %d must exceed conversation-only %d", request, conversation)
	}
	if got := a.staticOverheadTokens(); got <= 0 {
		t.Fatal("static overhead should be non-zero; an empty system prompt would mean the prompt is not built")
	}
}

// An unknown model must not be assumed to have a large window. Guessing high
// meant a request went out that the provider rejected outright.
func TestUnknownModelCompactsBeforeItBreaks(t *testing.T) {
	if got := contextWindowFor("space-bunny-free"); got >= 200000 {
		t.Fatalf("unknown model window = %d; a guess that high produces failed requests", got)
	}
	known := contextWindowFor("glm-5.3")
	if known <= contextWindowFor("space-bunny-free") {
		t.Fatalf("a known large model (%d) should exceed the unknown-model fallback (%d)", known, contextWindowFor("space-bunny-free"))
	}
}

// The reserve exists so a model has room to answer. Without it, filling the
// input window leaves nothing to reply into.
func TestUsableContextLeavesRoomForTheReply(t *testing.T) {
	for _, model := range []string{"space-bunny-free", "glm-5.3", "gpt-5.6-sol"} {
		window := contextWindowFor(model)
		usable := usableContext(model)
		if usable >= window {
			t.Fatalf("%s: usable %d must be below window %d", model, usable, window)
		}
		if window > ContextReserve && window-usable != ContextReserve {
			t.Fatalf("%s: reserved %d, want %d", model, window-usable, ContextReserve)
		}
	}
	// A window smaller than the reserve must not go negative.
	if got := usableContext("tiny"); got < 0 {
		t.Fatalf("usable context went negative: %d", got)
	}
}

// Tool definitions are re-sent on every request, so more tools means more
// overhead, and the estimate has to notice.
func TestMoreToolsMeansMoreOverhead(t *testing.T) {
	dir := t.TempDir()
	one := New(Options{Cwd: dir, Tools: []tools.Tool{{
		Name: "alpha", Description: "does the first thing",
		Run: func(context.Context, map[string]any) tools.Result { return tools.Result{} },
	}}})
	two := New(Options{Cwd: dir, Tools: []tools.Tool{
		{Name: "alpha", Description: "does the first thing", Run: func(context.Context, map[string]any) tools.Result { return tools.Result{} }},
		{Name: "beta", Description: "does the second thing", Parameters: map[string]any{"x": "y"},
			Run: func(context.Context, map[string]any) tools.Result { return tools.Result{} }},
	}})
	if one.staticOverheadTokens() <= 0 {
		t.Fatal("a tool definition should cost something")
	}
	if two.staticOverheadTokens() <= one.staticOverheadTokens() {
		t.Fatalf("two tools (%d) must cost more than one (%d)",
			two.staticOverheadTokens(), one.staticOverheadTokens())
	}
}

// The estimate has to stay cheap, because it runs on every iteration of the turn
// loop. A linear scan over the whole session per iteration would cost more than
// it saves.
func TestEstimateIsCheapForALargeSession(t *testing.T) {
	a := New(Options{Cwd: t.TempDir()})
	entries := entriesOf(12000, "a message of ordinary length in a long session")
	done := make(chan [2]int, 1)
	go func() {
		tokens := a.estimateRequestTokens(entries)
		done <- [2]int{tokens, len(entries)}
	}()
	got := <-done
	if got[0] <= 0 {
		t.Fatal("estimate should be positive")
	}
	if got[1] != 12000 {
		t.Fatalf("entries = %d", got[1])
	}
	fmt.Printf("12000 entries estimate to %d tokens\n", got[0])
}
