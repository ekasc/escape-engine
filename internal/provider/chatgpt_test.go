package provider

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestWireInput(t *testing.T) {
	in := []Message{
		{Role: "system", Text: "sys"},
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Args: map[string]any{"cmd": "ls"}}}},
		{Role: "tool", ToolCallID: "c1", Text: "done"},
	}
	out := wireInput(in)
	if len(out) != 4 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0]["type"] != "message" || out[1]["role"] != "user" {
		t.Fatalf("user/system wire: %+v", out[:2])
	}
	asst := out[2]
	calls, _ := asst["tool_calls"].([]map[string]any)
	if len(calls) != 1 || calls[0]["name"] != "bash" || calls[0]["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("assistant tool_calls wire: %+v", calls)
	}
	tool := out[3]
	if tool["type"] != "function_call_output" || tool["call_id"] != "c1" {
		t.Fatalf("tool wire: %+v", tool)
	}
}

func TestWireResponsesTools(t *testing.T) {
	out := wireResponsesTools([]ToolSpec{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}})
	if len(out) != 1 || out[0]["name"] != "bash" {
		t.Fatalf("tools wire: %+v", out)
	}
	if out[0]["type"] != "function" {
		t.Fatalf("tools wire type: %+v", out[0]["type"])
	}
}

func TestChatGPTStreamTranslation(t *testing.T) {
	sse := strings.Join([]string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":{\"text\":\"Hel\"}}",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":{\"text\":\"lo\"}}",
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":{\"text\":\"thinking...\"}}",
		"data: {\"type\":\"response.function_call_arguments.done\",\"delta\":{\"id\":\"fc_1\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"/x\\\"}\"}}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"output_tokens\":9,\"total_tokens\":59,\"input_tokens_details\":{\"cached_tokens\":20}}}}",
	}, "\n")
	s := &chatgptStream{sc: bufio.NewScanner(strings.NewReader(sse))}

	events := []Event{}
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d: %+v", len(events), events)
	}
	if events[0].Kind != EventText || events[0].Text != "Hel" {
		t.Fatalf("ev0 = %+v", events[0])
	}
	if events[1].Kind != EventText || events[1].Text != "lo" {
		t.Fatalf("ev1 = %+v", events[1])
	}
	if events[2].Kind != EventThinking || events[2].Thinking != "thinking..." {
		t.Fatalf("ev2 = %+v", events[2])
	}
	tc := events[3]
	if tc.Kind != EventToolCall || tc.ToolCall.Name != "read" || tc.ToolCall.Args["path"] != "/x" {
		t.Fatalf("ev3 = %+v", tc)
	}
	done := events[4]
	if done.Kind != EventDone || done.StopReason != "stop" {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage.TotalTokens != 59 || done.Usage.CacheRead != 20 {
		t.Fatalf("done usage = %+v", done.Usage)
	}
}

func TestChatGPTStreamCompletionUsage(t *testing.T) {
	sse := strings.Join([]string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":{\"text\":\"ok\"}}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}",
	}, "\n")
	s := &chatgptStream{sc: bufio.NewScanner(strings.NewReader(sse))}
	var last Event
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = ev
	}
	if last.Kind != EventDone || last.StopReason != "stop" {
		t.Fatalf("last = %+v", last)
	}
	if last.Usage.TotalTokens != 12 || last.Usage.Input != 10 || last.Usage.Output != 2 {
		t.Fatalf("usage = %+v", last.Usage)
	}
}

func TestChatGPTStreamFailed(t *testing.T) {
	sse := `data: {"type":"response.failed","error":{"message":"quota exceeded"}}`
	s := &chatgptStream{sc: bufio.NewScanner(strings.NewReader(sse))}
	if _, err := s.Next(); err == nil || err.Error() != "quota exceeded" {
		t.Fatalf("err = %v", err)
	}
}
