package provider

import "testing"

func TestEffortLadder(t *testing.T) {
	cases := map[string]string{
		"medium":  "low",
		"low":     "minimal",
		"high":    "medium",
		"minimal": "", // bottom: drop reasoning
		"weird":   "",
	}
	for in, want := range cases {
		if got := nextLowerEffort(in); got != want {
			t.Errorf("nextLowerEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsReasoningRejection(t *testing.T) {
	if !isReasoningRejection(400, []byte(`{"error":"unsupported reasoning_effort level"}`)) {
		t.Fatal("reasoning rejection should be detected")
	}
	if isReasoningRejection(500, []byte(`{"error":"server error"}`)) {
		t.Fatal("5xx is not a reasoning rejection")
	}
	if isReasoningRejection(400, []byte(`{"error":"rate limited"}`)) {
		t.Fatal("non-reasoning 400 should not trigger the ladder")
	}
}
