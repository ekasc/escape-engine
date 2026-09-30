package design

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The rules live here, in the engine, rather than in the shell. They are the
// creative half of Design Mode: everything the runner does is mechanical, and
// everything opinionated is a string in this file.

// sharedRules is the part of the brief every phase gets. It is short on purpose.
// The model is a capable agent with the repository open; a long preamble makes
// it verbose rather than better.
const sharedRules = `You are designing and building a web page. Work autonomously: do not ask questions, and do not stop to confirm. Where the request is ambiguous, choose the reading you would defend in review and record it as an assumption.

Reply with exactly one JSON object and nothing else. No prose before or after it, no markdown fence. Every field listed below is required; use an empty array or string rather than omitting a field.`

// phaseRules is the contract for each phase that produces an artifact. The keys
// match the JSON tags in phase.go, so a change to a struct and a change to its
// rules are the same edit.
var phaseRules = map[Phase]string{
	PhaseQualify: `Decide whether this request is a page design task at all. If it is not, say so plainly in "goal" and leave "audience" empty.

{
  "goal": "what the page is for, in one sentence",
  "audience": "who it is for, in one sentence",
  "constraints": ["hard requirement", "another"],
  "assumptions": ["what you decided because the request did not say"]
}`,

	PhaseBrief: `Write the brief. This is a hard gate: nothing gets built until this exists and is right.

{
  "summary": "the page in two sentences",
  "sections": ["ordered section names, top to bottom"],
  "mood": "the feeling, in a few words",
  "must_avoid": ["cliché or failure mode to steer clear of"]
}`,

	PhaseBrand: `Choose the visual system. Every later phase inherits these decisions rather than re-making them, so commit to them.

Palette entries are hex, exactly #rrggbb. Three to five is right.

{
  "name": "the product or site name",
  "palette": ["#0b0d10", "#8ab4f8"],
  "type_scale": "how type is sized and weighted across the page",
  "mood": ["three or four adjectives"]
}`,

	PhasePage: `Write the blueprint and the final copy. The copy is final: the build phase writes what you write here, it does not invent copy of its own. "uses_asset" lists the asset ids you will need, matched to blocks.

{
  "route": "/",
  "blocks": ["ordered block ids, top to bottom"],
  "copy": ["final copy per block, in the same order"],
  "uses_asset": ["asset id per block that needs one"]
}`,

	PhaseAssets: `Inventory one image the page needs, as a path inside the project. Use an existing file if the repository has one; otherwise give the path the build phase should write.

{
  "path": "public/hero.png",
  "alt": "what the image shows, for someone who cannot see it",
  "kind": "screenshot | photo | diagram | texture"
}`,

	PhaseBuild: `Build the page now. Use the tools to write the real files.

Work in the site directory given below. Produce a single self-contained page: one HTML file, its CSS, and any images. No framework, no build step, no package install. The page must work when opened directly from disk.

Match the brief, brand and page you were given. Follow the brand's palette and type scale exactly. Look at what you wrote before you finish.

When the files are on disk, reply with the list you wrote.

{
  "files": ["index.html", "style.css"],
  "notes": "one line on what you built and anything you left out"
}`,

	PhaseReview: `You are reviewing a page you were just shown as two screenshots: one at desktop width, one at mobile width. Read both image files.

Review it as a designer who would have to ship it. Be specific and concrete: name the block, say what is wrong, say what it should be instead. Do not invent problems to look thorough; an empty "blocking" list with a "pass" verdict is a real answer.

If it needs work, set "from" to the earliest phase whose output has to change. A palette problem is "brand". Copy and layout are "page". Wrong structure is "brief".

{
  "pass": 0,
  "verdict": "pass | repair",
  "blocking": ["what is wrong, concretely"],
  "from": "brief | brand | page | assets",
  "why": "one line, empty when passing"
}`,
}

// phaseOrder is the sequence a prompt chain follows, used to assemble the prior
// context a phase is given.
var phaseOrder = []Phase{PhaseQualify, PhaseBrief, PhaseBrand, PhasePage, PhaseAssets}

// prior renders the artifacts already accepted, so a later phase builds on them
// instead of re-deciding them. A phase with no accepted predecessor gets an empty
// string rather than a placeholder, so the prompt does not teach the model that
// an empty artifact is normal.
func prior(s *Store, upTo Phase) string {
	var b strings.Builder
	for _, p := range phaseOrder {
		if p == upTo {
			break
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, artifactName(p)))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (already agreed, do not revisit)\n%s\n", p, raw)
	}
	return b.String()
}

// phasePrompt assembles the instruction for one artifact-producing phase.
func phasePrompt(s *Store, p Phase, siteDir, request string, last error) string {
	var b strings.Builder
	b.WriteString(sharedRules)
	if p == PhaseQualify && strings.TrimSpace(request) != "" {
		// Only qualify needs it, and only because it is the phase that reads it.
		// Every later phase learns what was asked from the artifacts in front
		// of it, which is the same thing said once rather than restated.
		fmt.Fprintf(&b, "\n\n## The request\n%s\n", strings.TrimSpace(request))
	}
	b.WriteString("\n\n## This phase: ")
	b.WriteString(string(p))
	b.WriteString("\n\n")
	b.WriteString(phaseRules[p])
	if ctx := prior(s, p); ctx != "" {
		b.WriteString("\n\n## What is already decided\n")
		b.WriteString(ctx)
	}
	if p == PhaseBuild {
		fmt.Fprintf(&b, "\n\n## Where to build\nWrite the site in %q, relative to the project root. The directory may not exist; create it.\n", siteDir)
	}
	if last != nil {
		fmt.Fprintf(&b, "\n\n## Your previous reply was rejected\n%s\nReturn a corrected object. Fix that and nothing else.\n", last)
	}
	return b.String()
}

// reviewPrompt assembles the review instruction, naming the screenshots the
// model must read. The paths are given rather than the bytes because the agent
// already has a tool that attaches an image, and reusing it means the review
// sees exactly what a human would see when they open the same file.
func reviewPrompt(s *Store, pass int, last error) string {
	var b strings.Builder
	b.WriteString(sharedRules)
	fmt.Fprintf(&b, "\n\n## This phase: review, pass %d\n\nRead both screenshots before you answer:\n\n- %s\n- %s\n\n",
		pass, screenshotPath(s, Desktop), screenshotPath(s, Mobile))
	b.WriteString(phaseRules[PhaseReview])
	if pass > 0 {
		fmt.Fprintf(&b, "\n\n## Earlier passes\nThis is repair pass %d of at most %d. The page has already been through review. Fix what is named and leave the rest of the design alone.\n", pass, MaxRepairs)
	}
	if last != nil {
		fmt.Fprintf(&b, "\n\n## Your previous reply was rejected\n%s\nReturn a corrected object.\n", last)
	}
	return b.String()
}

// screenshotPath is where a viewport's shot lives inside a run's directory.
func screenshotPath(s *Store, v Viewport) string {
	return s.dir + "/" + shotFile(v)
}
