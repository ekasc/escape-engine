package session

import "testing"

func TestAcceptsImage(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5.6-sol", true},
		{"gpt-5.6-luna", true},
		{"gpt-5.6-terra", true},
		{"minimax-m3", true},
		{"deepseek-v4-flash", false},
		{"deepseek-v4-pro", false},
		{"glm-5", false},
		{"glm-5-7b", false},
		{"gpt-5.6-omega", false},
		{"unknown-model", false},
		{"", false},
	}
	for _, c := range cases {
		if got := LookupModel(c.model).AcceptsImage(); got != c.want {
			t.Errorf("LookupModel(%q).AcceptsImage() = %v, want %v", c.model, got, c.want)
		}
	}
}

// An unknown id must never be treated as vision-capable. Design Mode attaches a
// screenshot for review, and a model that cannot read it would answer from the
// surrounding text instead.
func TestUnknownModelIsTextOnly(t *testing.T) {
	if LookupModel("some-model-nobody-catalogued").AcceptsImage() {
		t.Error("unknown model reported image support")
	}
	if len(DefaultModelMeta.Input) == 0 {
		t.Error("DefaultModelMeta declares no input modality")
	}
}
