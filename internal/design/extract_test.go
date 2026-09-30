package design

import (
	"strings"
	"testing"
)

func TestExtractJSONObjectSurvivesHowModelsActuallyReply(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
	}{
		{"bare", `{"goal":"a page"}`, "a page"},
		{"fenced", "```json\n{\"goal\":\"a page\"}\n```", "a page"},
		{"fenced no language", "```\n{\"goal\":\"a page\"}\n```", "a page"},
		{"preamble", "Here is the brief you asked for.\n\n{\"goal\":\"a page\"}\n\nLet me know if you want changes.", "a page"},
		{"preamble and fence", "Sure.\n\n```json\n{\"goal\":\"a page\"}\n```\n\nDone.", "a page"},
		{"braces inside strings", `{"goal":"use { and } freely","audience":"devs"}`, "use { and } freely"},
		{"escaped quote then brace", `{"goal":"say \"hi\" }","audience":"devs"}`, `say "hi" }`},
		{"nested object", `{"goal":"a page","extra":{"x":1}}`, "a page"},
		{"trailing prose with braces", `{"goal":"a page"} note: {this is prose}`, "a page"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			art, err := parseArtifact(PhaseQualify, c.reply)
			if err != nil {
				t.Fatalf("parseArtifact: %v", err)
			}
			got, ok := art.(*Qualify)
			if !ok {
				t.Fatalf("artifact = %T, want *Qualify", art)
			}
			if got.Goal != c.want {
				t.Errorf("goal = %v, want %q", got.Goal, c.want)
			}
		})
	}
}

func TestExtractJSONObjectRejectsWhatItCannotUse(t *testing.T) {
	for _, reply := range []string{
		"",
		"I cannot help with that.",
		`{"goal": "unterminated`,
		"```json\nstill thinking\n```",
	} {
		if got := jsonObjects(reply); len(got) != 0 {
			t.Errorf("jsonObjects(%q) = %q, want nothing", reply, got)
		}
	}
}

// A reply that mentions braces in prose before the real object is common
// enough that taking the first brace would spend a retry on a correct answer.
func TestParseArtifactSkipsProseThatContainsABrace(t *testing.T) {
	reply := "Here is the {variant} you asked for:\n```json\n" +
		`{"summary":"one page","sections":["hero"]}` + "\n```\nHope that helps."

	art, err := parseArtifact(PhaseBrief, reply)
	if err != nil {
		t.Fatalf("a correct answer behind a brace in prose was rejected: %v", err)
	}
	if art.phase() != PhaseBrief {
		t.Errorf("phase = %q, want brief", art.phase())
	}
}

// Requiring validation is what makes scanning safe: a fragment that happens to
// be well-formed JSON still has to satisfy the schema before it is believed.
func TestParseArtifactDoesNotAcceptAWellFormedFragment(t *testing.T) {
	reply := `{"unrelated": true}` + "\n" + `{"summary":"one page","sections":["hero"]}`

	art, err := parseArtifact(PhaseBrief, reply)
	if err != nil {
		t.Fatal(err)
	}
	brief, ok := art.(*Brief)
	if !ok {
		t.Fatalf("artifact = %T, want *Brief", art)
	}
	if brief.Summary != "one page" {
		t.Errorf("summary = %q, want the second object, not the fragment", brief.Summary)
	}
}

func TestParseArtifactValidatesBeforeReturning(t *testing.T) {
	// A reply that parses as JSON but is not a usable brief must be rejected
	// here, not stored and discovered later.
	_, err := parseArtifact(PhaseBrief, `{"summary":"","sections":[]}`)
	if err == nil {
		t.Fatal("accepted a brief with no summary and no sections")
	}
	if !strings.Contains(err.Error(), "brief") {
		t.Errorf("error = %v, want it to name the phase", err)
	}

	art, err := parseArtifact(PhaseBrief, "prose\n```json\n"+`{"summary":"one page","sections":["hero"]}`+"\n```")
	if err != nil {
		t.Fatal(err)
	}
	if art.phase() != PhaseBrief {
		t.Errorf("phase = %q, want brief", art.phase())
	}
}

func TestParseArtifactRefusesPhasesThatProduceNothing(t *testing.T) {
	if _, err := parseArtifact(PhasePreview, `{"a":1}`); err == nil {
		t.Error("parsed an artifact for a phase that produces none")
	}
}

func TestRepairBudgetStopsTheChain(t *testing.T) {
	// The repair bound is a property of the verdict, not of a counter the
	// runner keeps, so it is checked at the boundary where the verdict lands.
	over := Review{Pass: MaxRepairs, Verdict: VerdictRepair, From: PhasePage, Why: "still wrong"}
	if err := over.validate(); err != nil {
		t.Errorf("a repair verdict at the budget limit was rejected: %v", err)
	}
	beyond := Review{Pass: MaxRepairs + 1, Verdict: VerdictRepair, From: PhasePage, Why: "still wrong"}
	if err := beyond.validate(); err == nil {
		t.Error("accepted a verdict past the repair budget")
	}
}
