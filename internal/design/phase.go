// Package design holds the contracts and machinery a Design Mode run is built
// from: the phase sequence, the artifacts each phase produces, the store they
// live in, and the two runners that serve and photograph a built page.
//
// Orchestration is not here. This package answers three questions for whatever
// drives a run: which phase comes next, is this artifact acceptable, and how do
// I take a picture of this page. A run's position is derived from the artifacts
// on disk, so a run interrupted mid-pipeline resumes by asking the same
// question it asks when it starts.
package design

import (
	"fmt"
	"strings"
)

// Phase names one step of a design run.
type Phase string

const (
	PhaseQualify Phase = "qualify"
	PhaseBrief   Phase = "brief"
	PhaseBrand   Phase = "brand"
	PhasePage    Phase = "page"
	PhaseAssets  Phase = "assets"
	PhaseBuild   Phase = "build"
	PhasePreview Phase = "preview"
	PhaseCapture Phase = "capture"
	PhaseReview  Phase = "review"
	// PhaseDone is the position of a run whose artifacts are all present and
	// valid. It is not a step.
	PhaseDone Phase = "done"
)

// MaxRepairs is the number of repair passes a run may take after its first
// review. The bound is the point: a run that cannot be repaired reports that it
// failed rather than spending a budget nobody set.
const MaxRepairs = 2

// phaseSpec is one row of the run's state machine.
type phaseSpec struct {
	phase Phase
	// artifact is the file that proves the phase ran, empty for phases that
	// leave no durable trace.
	//
	// Preview is the only one, and unavoidably so: what it produces is a bound
	// port number, which is not true across a restart. Persisting it would mean
	// persisting something that has already stopped being true, so a preview
	// record is a lie with a timestamp on it. Capture's artifact is the pair of
	// screenshots themselves, so it needs no index file of its own.
	artifact string
}

// phases is the ordered sequence. Its order is the entire state machine: the
// store walks it to find a run's position, so there is no second place that
// could disagree about what comes next.
var phases = []phaseSpec{
	{PhaseQualify, "qualify.json"},
	{PhaseBrief, "brief.json"},
	{PhaseBrand, "brand.json"},
	{PhasePage, "page.json"},
	{PhaseAssets, "assets.json"},
	{PhaseBuild, "build.json"},
	{PhasePreview, ""},
	{PhaseCapture, ""},
	{PhaseReview, ""}, // numbered per pass, see Store.Repairs
}

// artifact is the closed set of things a phase can produce. The unexported
// method keeps it closed: a payload cannot exist without a phase, and no
// package outside this one can add an arm.
type artifact interface {
	phase() Phase
	validate() error
}

// InvalidError reports which field of which artifact was rejected, so a run can
// tell the model what to fix instead of that it was wrong.
type InvalidError struct {
	Phase Phase
	Field string
	Why   string
}

func (e *InvalidError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s artifact rejected: %s", e.Phase, e.Why)
	}
	return fmt.Sprintf("%s.%s: %s", e.Phase, e.Field, e.Why)
}

func reject(p Phase, field, why string) error {
	return &InvalidError{Phase: p, Field: field, Why: why}
}

// Qualify is the answer to whether this request is a design task at all, and
// what it is asking for. A run that cannot fill this in stops rather than
// inventing a brief for something that was not a design request.
type Qualify struct {
	Goal        string   `json:"goal"`
	Audience    string   `json:"audience"`
	Constraints []string `json:"constraints"`
	Assumptions []string `json:"assumptions"`
}

// Brief is the gated summary of what gets built. No build starts without one.
type Brief struct {
	Summary   string   `json:"summary"`
	Sections  []string `json:"sections"`
	Mood      string   `json:"mood"`
	MustAvoid []string `json:"must_avoid"`
}

// Brand is the visual decision set: the decisions every later phase inherits
// rather than re-derives.
type Brand struct {
	Name      string   `json:"name"`
	Palette   []string `json:"palette"`
	TypeScale string   `json:"type_scale"`
	Mood      []string `json:"mood"`
}

// Page is the blueprint and the final copy. Copy lives here rather than in the
// build phase so the build has nothing to invent.
type Page struct {
	Route     string   `json:"route"`
	Blocks    []string `json:"blocks"`
	Copy      []string `json:"copy"`
	UsesAsset []string `json:"uses_asset"`
}

// Assets is the inventory of images and fonts the page is allowed to reference.
type Assets struct {
	Path string `json:"path"`
	Alt  string `json:"alt"`
	Kind string `json:"kind"`
}

// Build records what the build pass wrote, which is what a repair pass needs to
// know it is allowed to replace.
type Build struct {
	Files []string `json:"files"`
	Notes string   `json:"notes"`
}

// Review is one pass's verdict. Pass is zero for the first review, and a run
// that has taken MaxRepairs repairs cannot produce another.
type Review struct {
	Pass     int      `json:"pass"`
	Verdict  string   `json:"verdict"`
	Blocking []string `json:"blocking"`
	From     Phase    `json:"from"`
	Why      string   `json:"why"`
}

// Verdict values. A review that does not pass is not a failure of the run; it is
// the run asking for a repair, and the budget is what stops that asking forever.
const (
	VerdictPass   = "pass"
	VerdictRepair = "repair"
)

func (q Qualify) phase() Phase { return PhaseQualify }
func (b Brief) phase() Phase   { return PhaseBrief }
func (b Brand) phase() Phase   { return PhaseBrand }
func (p Page) phase() Phase    { return PhasePage }
func (a Assets) phase() Phase  { return PhaseAssets }
func (b Build) phase() Phase   { return PhaseBuild }
func (r Review) phase() Phase  { return PhaseReview }

func (q Qualify) validate() error {
	return requireText(PhaseQualify, "goal", q.Goal)
}

func (b Brief) validate() error {
	if err := requireText(PhaseBrief, "summary", b.Summary); err != nil {
		return err
	}
	return requireSome(PhaseBrief, "sections", b.Sections)
}

func (b Brand) validate() error {
	if err := requireText(PhaseBrand, "name", b.Name); err != nil {
		return err
	}
	for i, c := range b.Palette {
		if !isHexColor(c) {
			return reject(PhaseBrand, fmt.Sprintf("palette[%d]", i), fmt.Sprintf("want #rrggbb, got %q", c))
		}
	}
	return requireSome(PhaseBrand, "palette", b.Palette)
}

func (p Page) validate() error {
	if err := requireText(PhasePage, "route", p.Route); err != nil {
		return err
	}
	return requireSome(PhasePage, "blocks", p.Blocks)
}

func (a Assets) validate() error {
	if err := requireText(PhaseAssets, "path", a.Path); err != nil {
		return err
	}
	return requireText(PhaseAssets, "alt", a.Alt)
}

func (b Build) validate() error {
	return requireSome(PhaseBuild, "files", b.Files)
}

func (r Review) validate() error {
	switch r.Verdict {
	case VerdictPass:
		return nil
	case VerdictRepair:
		if r.Pass > MaxRepairs {
			return reject(PhaseReview, "pass", fmt.Sprintf("repair budget of %d is spent", MaxRepairs))
		}
		if !isPhase(r.From) || r.From == PhaseReview || r.From == PhaseDone {
			return reject(PhaseReview, "from", "must name the earliest phase that needs redoing")
		}
		return requireText(PhaseReview, "why", r.Why)
	default:
		return reject(PhaseReview, "verdict", "want "+VerdictPass+" or "+VerdictRepair)
	}
}

// NeedsRepair reports whether this review asks for another pass.
func (r Review) NeedsRepair() bool { return r.Verdict == VerdictRepair }

func requireText(p Phase, field, v string) error {
	if strings.TrimSpace(v) == "" {
		return reject(p, field, "must not be empty")
	}
	return nil
}

func requireSome(p Phase, field string, vs []string) error {
	if len(vs) == 0 {
		return reject(p, field, "must not be empty")
	}
	return nil
}

func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, r := range s[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func isPhase(p Phase) bool {
	for _, spec := range phases {
		if spec.phase == p {
			return true
		}
	}
	return false
}
