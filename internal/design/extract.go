package design

import (
	"encoding/json"
	"fmt"
	"strings"
)

// jsonObjects pulls every balanced JSON object out of a model's reply, in the
// order they appear.
//
// The reply is not the artifact. A model asked for JSON will hand it back
// inside a fence, after a sentence of preamble, or with a postscript, and
// refusing those replies would mean retrying a correct answer. So objects are
// located by scanning for balanced braces while tracking string state, and
// everything around them is discarded.
//
// Every candidate is returned rather than only the first, because a reply that
// opens with a brace in prose — "here is the {variant} you asked for" — would
// otherwise hand back that fragment and spend a retry on an answer that was
// already correct. The caller decides which candidate is the artifact by
// trying them against the phase's schema, so a stray object that happens to
// parse still has to pass validation.
func jsonObjects(text string) [][]byte {
	trimmed := stripFences(text)
	var found [][]byte
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != '{' {
			continue
		}
		end, err := matchBrace(trimmed, i)
		if err != nil {
			// Unbalanced from here to the end of the reply, so no later object
			// can be complete either.
			break
		}
		found = append(found, []byte(trimmed[i:end+1]))
		i = end
	}
	return found
}

// stripFences removes markdown code fences, keeping what is inside them. Only
// the markers go: a fenced object is the object, and discarding the body along
// with the fence would leave nothing to parse.
func stripFences(text string) string {
	var b strings.Builder
	rest := text
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		rest = rest[open+3:]
		// An opening fence may carry a language tag, which is a bare word on the
		// fence line rather than part of the object.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 && !strings.ContainsRune(rest[:nl], '{') {
			rest = rest[nl+1:]
		}
		closing := strings.Index(rest, "```")
		if closing < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:closing])
		rest = rest[closing+3:]
	}
}

// matchBrace returns the index of the brace closing the one at start, skipping
// braces that appear inside string literals. Without that, a copy field like
// "use { and }" ends the object early and the reply fails to parse for a reason
// that looks like the model's fault.
func matchBrace(s string, start int) (int, error) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("JSON object is not closed")
}

// newArtifact is the registry that turns a phase into a value the reply can be
// unmarshalled into. It is the same closed set as the artifact interface, held
// as constructors so a reply is parsed into a typed model before anything
// inspects it.
var newArtifact = map[Phase]func() artifact{
	PhaseQualify: func() artifact { return new(Qualify) },
	PhaseBrief:   func() artifact { return new(Brief) },
	PhaseBrand:   func() artifact { return new(Brand) },
	PhasePage:    func() artifact { return new(Page) },
	PhaseAssets:  func() artifact { return new(Assets) },
	PhaseBuild:   func() artifact { return new(Build) },
	PhaseReview:  func() artifact { return new(Review) },
}

// parseArtifact reads one phase's artifact out of a model reply. It parses at
// the boundary: everything downstream sees a validated value or an error naming
// what was wrong.
//
// Candidates are tried in the order they appear and the first one that both
// unmarshals and validates wins. Requiring validation is what makes this safe:
// a fragment that happens to be well-formed JSON still has to satisfy the
// phase's schema before it can be mistaken for the answer.
func parseArtifact(p Phase, reply string) (artifact, error) {
	mk, ok := newArtifact[p]
	if !ok {
		return nil, fmt.Errorf("phase %s produces no artifact", p)
	}
	candidates := jsonObjects(reply)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s: no JSON object in reply", p)
	}
	var last error
	for _, raw := range candidates {
		art := mk()
		if err := json.Unmarshal(raw, art); err != nil {
			last = fmt.Errorf("%s: reply is not valid JSON: %w", p, err)
			continue
		}
		if err := art.validate(); err != nil {
			last = err
			continue
		}
		return art, nil
	}
	return nil, last
}

// parseReview parses a review verdict as a value.
//
// It lives beside the registry rather than in the runner because the registry is
// what decides a review is constructed as a pointer. Every artifact type has
// value receivers, so both Review and *Review satisfy the interface and a
// caller asserting the wrong one gets a panic at run time instead of a compile
// error. Keeping the one assertion that matters next to the code that creates
// the value means there is a single place to be right.
func parseReview(reply string) (Review, error) {
	art, err := parseArtifact(PhaseReview, reply)
	if err != nil {
		return Review{}, err
	}
	rev, ok := art.(*Review)
	if !ok {
		return Review{}, fmt.Errorf("review: registry produced %T, want *Review", art)
	}
	return *rev, nil
}
