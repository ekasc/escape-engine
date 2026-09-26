package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageDataURI(t *testing.T) {
	im := &Image{MimeType: "image/png", Data: []byte{0x01, 0x02}}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})
	if got := im.DataURI(); got != want {
		t.Fatalf("DataURI = %q, want %q", got, want)
	}
}

func TestParseDataURI(t *testing.T) {
	data := []byte{0x89, 0x50, 0x4e, 0x47}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	im, err := ParseDataURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if im.MimeType != "image/png" {
		t.Errorf("mime = %q", im.MimeType)
	}
	if !bytesEq(im.Data, data) {
		t.Errorf("data = %v, want %v", im.Data, data)
	}
	if got := im.DataURI(); got != uri {
		t.Errorf("round trip = %q, want %q", got, uri)
	}
	for _, bad := range []string{
		"",
		"image/png;base64,AA==",     // no data: prefix
		"data:;base64,AA==",         // missing MIME type
		"data:image/png;base64,!!!", // bad base64
		"data:image/png;base64",     // no comma separator
	} {
		if _, err := ParseDataURI(bad); err == nil {
			t.Errorf("ParseDataURI(%q): expected error", bad)
		}
	}
}

func TestWireMessagesImageContentParts(t *testing.T) {
	im := &Image{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}
	w := wireMessages([]Message{{Role: "user", Text: "what is this", Image: im}})
	if len(w) != 1 {
		t.Fatalf("len = %d", len(w))
	}
	content, ok := w[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content = %T, want []map[string]any", w[0]["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts = %d, want 2", len(content))
	}
	if content[0]["type"] != "text" || content[0]["text"] != "what is this" {
		t.Errorf("text part = %v", content[0])
	}
	if content[1]["type"] != "image_url" {
		t.Fatalf("image part = %v", content[1])
	}
	iu, ok := content[1]["image_url"].(map[string]any)
	if !ok || iu["url"] != im.DataURI() {
		t.Errorf("image_url = %v, want url %q", content[1]["image_url"], im.DataURI())
	}
}

func TestWireMessagesNoImageKeepsStringContent(t *testing.T) {
	w := wireMessages([]Message{
		{Role: "user", Text: "hi"},
		{Role: "tool", ToolCallID: "c1", Text: "file"},
	})
	if w[0]["content"] != "hi" {
		t.Errorf("user content = %v", w[0]["content"])
	}
	if w[1]["content"] != "file" {
		t.Errorf("tool content = %v", w[1]["content"])
	}
}

func TestWireToolMessageWithImage(t *testing.T) {
	im := &Image{MimeType: "image/gif", Data: []byte{'G', 'I', 'F', '8'}}
	w := wireMessages([]Message{{Role: "tool", ToolCallID: "c1", Text: "screenshot", Image: im}})
	if w[0]["role"] != "tool" || w[0]["tool_call_id"] != "c1" {
		t.Fatalf("msg = %v", w[0])
	}
	content := w[0]["content"].([]map[string]any)
	if len(content) != 2 || content[1]["type"] != "image_url" {
		t.Fatalf("content = %v", content)
	}
}

func TestImageSentInRequest(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	im := &Image{MimeType: "image/png", Data: []byte{0x01, 0x02, 0x03}}
	c := NewOpenAI(srv.URL+"/v1", "k", "m")
	s, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Text: "see", Image: im}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		ev, err := s.Next()
		if err == io.EOF || ev.Kind == EventDone {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	first := msgs[0].(map[string]any)
	content := first["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v", content)
	}
	img := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part = %v", img)
	}
	iu := img["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,AQID" {
		t.Errorf("url = %v", iu["url"])
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
