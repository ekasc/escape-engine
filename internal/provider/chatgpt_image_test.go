package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeInput(t *testing.T, msgs []Message) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(wireInput(msgs))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWireInputSendsImagesToResponses(t *testing.T) {
	img := &Image{MimeType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}
	msgs := []Message{
		{Role: "user", Text: "review this", Image: img},
		{Role: "user", Text: "and this", Images: []*Image{img}},
	}
	in := decodeInput(t, msgs)

	for i, item := range in {
		content, ok := item["content"].([]any)
		if !ok || len(content) != 2 {
			t.Fatalf("item %d: content = %v, want text plus one image block", i, content)
		}
		block := content[1].(map[string]any)
		if block["type"] != "input_image" {
			t.Errorf("item %d: block type = %v, want input_image", i, block["type"])
		}
		url, _ := block["image_url"].(string)
		if !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Errorf("item %d: image_url = %q, want a data URI", i, url)
		}
		if content[0].(map[string]any)["type"] != "input_text" {
			t.Errorf("item %d: first block is not input_text", i)
		}
	}
}

// Without this the screenshot is silently dropped on the Responses path, and a
// Codex model reviews a page it was never shown.
func TestWireInputKeepsTextOnlyMessagesUnchanged(t *testing.T) {
	in := decodeInput(t, []Message{{Role: "user", Text: "hello"}})
	content := in[0]["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want a single text block", content)
	}
	if content[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("block = %v, want input_text", content[0])
	}
}

func TestWireInputOrdersSingularImageFirst(t *testing.T) {
	first := &Image{MimeType: "image/png", Data: []byte("first")}
	second := &Image{MimeType: "image/jpeg", Data: []byte("second")}
	in := decodeInput(t, []Message{{Role: "user", Text: "two", Image: first, Images: []*Image{second}}})

	content := in[0]["content"].([]any)
	got := []string{
		content[1].(map[string]any)["image_url"].(string),
		content[2].(map[string]any)["image_url"].(string),
	}
	if !strings.Contains(got[0], base64Of(first)) {
		t.Errorf("first block = %q, want the singular image", got[0])
	}
	if !strings.Contains(got[1], base64Of(second)) {
		t.Errorf("second block = %q, want the images slice entry", got[1])
	}
}

func base64Of(im *Image) string {
	full := im.DataURI()
	_, payload, _ := strings.Cut(full, ",")
	return payload
}
