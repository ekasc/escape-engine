package session

import "testing"

func TestFuzzyMatchIndicesAddressTheCandidate(t *testing.T) {
	// The indices must land on the characters the query used, in the string the
	// caller shows, or the highlight explains a ranking it did not produce.
	_, idx, ok := FuzzyMatchIndices("aoi", "ts/aoi")
	if !ok {
		t.Fatal("aoi should match ts/aoi")
	}
	got := ""
	for _, i := range idx {
		got += string("ts/aoi"[i])
	}
	if got != "aoi" {
		t.Errorf("indices %v spell %q, want %q", idx, got, "aoi")
	}
}

func TestFuzzyMatchIndicesAcrossSegments(t *testing.T) {
	_, idx, ok := FuzzyMatchIndices("taoi", "ts/aoi-website")
	if !ok {
		t.Fatal("taoi should match ts/aoi-website")
	}
	got := ""
	for _, i := range idx {
		got += string("ts/aoi-website"[i])
	}
	if got != "taoi" {
		t.Errorf("indices %v spell %q, want %q", idx, got, "taoi")
	}
}

func TestFuzzyMatchAndIndicesAgreeOnTheScore(t *testing.T) {
	// Adding indices must not have moved the ranking. FuzzyMatch is the wrapper,
	// so the two scoring the same candidate have to produce the same number.
	for _, c := range []struct{ query, candidate string }{
		{"eng", "engine"},
		{"eng", "notes/engineering-notes"},
		{"aoi", "ts/aoi-website"},
		{"x", "a/b/c/d/e"},
	} {
		want, okWant := FuzzyMatch(c.query, c.candidate)
		got, _, okGot := FuzzyMatchIndices(c.query, c.candidate)
		if okWant != okGot || want != got {
			t.Errorf("%q/%q: FuzzyMatch=(%d,%v) Indices=(%d,%v)",
				c.query, c.candidate, want, okWant, got, okGot)
		}
	}
}

func TestFuzzyMatchIndicesEdgeCases(t *testing.T) {
	if _, _, ok := FuzzyMatchIndices("", "anything"); !ok {
		t.Error("an empty query should match everything")
	}
	if _, _, ok := FuzzyMatchIndices("zzz", "abc"); ok {
		t.Error("a non-matching query should not match")
	}
	// A rejected match must not leave indices behind, or a client would
	// highlight characters in a result it is about to be told is not a match.
	if _, idx, ok := FuzzyMatchIndices("zzz", "abc"); ok || idx != nil {
		t.Errorf("a non-match returned indices %v", idx)
	}
}
