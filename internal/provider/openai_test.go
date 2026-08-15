package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveSSE spins up a fake OpenAI endpoint that returns the given SSE body.
func serveSSE(t *testing.T, body string, status int) (*OpenAI, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewOpenAI(srv.URL+"/v1", "test-key", "test-model"), srv
}

func TestStreamTextAndDone(t *testing.T) {
	c, _ := serveSSE(t, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", http.StatusOK)
	s, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var text strings.Builder
	var gotDone bool
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Text)
		case EventDone:
			gotDone = true
			if ev.StopReason != "stop" {
				t.Errorf("stop reason = %q", ev.StopReason)
			}
		}
	}
	if text.String() != "Hello" || !gotDone {
		t.Fatalf("text=%q done=%v", text.String(), gotDone)
	}
}

func TestStreamToolCallReassembly(t *testing.T) {
	// A tool call split across chunks with fragmentary arguments JSON.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	c, _ := serveSSE(t, body, http.StatusOK)
	s, err := c.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var tc *ToolCall
	gotDone := false
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case EventToolCall:
			x := ev.ToolCall
			tc = &x
		case EventDone:
			gotDone = true
			if ev.StopReason != "tool_calls" {
				t.Errorf("stop reason = %q", ev.StopReason)
			}
		}
	}
	if tc == nil || !gotDone {
		t.Fatalf("tool call=%v done=%v", tc, gotDone)
	}
	if tc.Name != "read" || tc.ID != "call_1" {
		t.Errorf("tool call = %+v", tc)
	}
	if got := tc.Args["path"]; got != "main.go" {
		t.Errorf("args path = %v", got)
	}
}

func TestStreamThinking(t *testing.T) {
	c, _ := serveSSE(t, `data: {"choices":[{"delta":{"reasoning_content":"hmm"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`, http.StatusOK)
	s, _ := c.Stream(context.Background(), Request{})
	defer s.Close()

	var thinking, text string
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case EventThinking:
			thinking += ev.Thinking
		case EventText:
			text += ev.Text
		}
	}
	if thinking != "hmm" || text != "answer" {
		t.Fatalf("thinking=%q text=%q", thinking, text)
	}
}

func TestRetryableOn503(t *testing.T) {
	c, _ := serveSSE(t, "", http.StatusServiceUnavailable)
	_, err := c.Stream(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var re RetryableError
	if !asRetryable(err, &re) {
		t.Fatalf("expected RetryableError, got %T: %v", err, err)
	}
	if re.Status != 503 {
		t.Errorf("status = %d", re.Status)
	}
}

func TestUsageReported(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"x"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`
	c, _ := serveSSE(t, body, http.StatusOK)
	s, _ := c.Stream(context.Background(), Request{})
	defer s.Close()

	var u Usage
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == EventDone {
			u = ev.Usage
		}
	}
	if u.Input != 10 || u.Output != 5 || u.TotalTokens != 15 {
		t.Errorf("usage = %+v", u)
	}
}

func TestWireMessagesToolRoundTrip(t *testing.T) {
	msgs := []Message{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "let me", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Args: map[string]any{"command": "ls"}}}},
		{Role: "tool", ToolCallID: "c1", Text: "file"},
	}
	w := wireMessages(msgs)
	if len(w) != 3 {
		t.Fatalf("len = %d", len(w))
	}
	if w[1]["tool_calls"] == nil {
		t.Fatal("missing tool_calls on assistant")
	}
	fn := w[1]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("args not valid JSON: %v", err)
	}
	if args["command"] != "ls" {
		t.Errorf("args = %v", args)
	}
	if w[2]["role"] != "tool" || w[2]["tool_call_id"] != "c1" {
		t.Errorf("tool msg = %v", w[2])
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Handler: func(ctx context.Context, req Request) ([]Event, error) {
		if len(req.Messages) == 0 {
			return nil, &RetryableError{Status: 429}
		}
		return []Event{{Kind: EventText, Text: "ok"}, {Kind: EventDone, StopReason: "stop"}}, nil
	}}
	if _, err := f.Stream(context.Background(), Request{}); !asRetryable(err, &RetryableError{}) {
		t.Fatal("expected retryable error")
	}
	s, err := f.Stream(context.Background(), Request{Messages: []Message{{Role: "user"}}})
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := s.Next()
	if ev.Kind != EventText || ev.Text != "ok" {
		t.Errorf("ev = %+v", ev)
	}
	if f.Calls != 2 {
		t.Errorf("calls = %d", f.Calls)
	}
}

func asRetryable(err error, target *RetryableError) bool {
	var found *RetryableError
	for err != nil {
		if re, ok := err.(*RetryableError); ok {
			found = re
			break
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	if found == nil {
		return false
	}
	if target != nil {
		*target = *found
	}
	return true
}
