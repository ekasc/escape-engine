package design

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store owns one run's directory under <root>/.escape/design/<sessionID>.
//
// A run has no state file and no cursor. Where it got to is derived, on demand,
// from which artifacts are present and valid, so a run killed between two
// phases resumes by asking the same question it asks when it starts. That also
// means a hand-edited or half-written artifact cannot claim a phase it did not
// finish: the walk re-validates what it finds.
type Store struct {
	root string
	dir  string
}

// NewStore returns the store for a session, creating its directory. The root is
// the workspace, and the session id must be a single path segment.
func NewStore(root, sessionID string) (*Store, error) {
	if !validSessionID(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: abs, dir: filepath.Join(abs, ".escape", "design", sessionID)}, nil
}

// Dir is the run's directory. It may not exist yet; Save creates it.
func (s *Store) Dir() string { return s.dir }

// Save validates an artifact and writes it atomically. A rejected artifact
// leaves the store untouched, so a bad model response cannot advance a run.
func (s *Store) Save(a artifact) error {
	if err := a.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(s.dir, s.fileFor(a)), data)
}

// fileFor is the artifact's filename, which is where the store's own naming
// rules live. Review is the one artifact numbered per pass, because the number
// of review files is how a run counts the repairs it has spent.
//
// Both Review and *Review are accepted. Every artifact type has value
// receivers, so both satisfy the interface, and a type assertion that only
// matched one of them would send the other down the generic path and write
// "review.json" — a name the store never counts, so the run would silently lose
// its repair history rather than fail.
func (s *Store) fileFor(a artifact) string {
	switch r := a.(type) {
	case Review:
		return reviewFile(r.Pass)
	case *Review:
		return reviewFile(r.Pass)
	}
	for _, spec := range phases {
		if spec.phase == a.phase() && spec.artifact != "" {
			return spec.artifact
		}
	}
	return string(a.phase()) + ".json"
}

func reviewFile(pass int) string { return "review-" + strconv.Itoa(pass) + ".json" }

// Load reads and validates a phase's artifact. A phase whose output is not a
// single file (preview, capture, review) is unsupported here; read those through
// what they are, LoadReview for a pass, the screenshots for a capture.
func Load[T any, PT interface {
	*T
	artifact
}](s *Store, p Phase) (T, error) {
	var zero T
	name := artifactName(p)
	if name == "" {
		return zero, fmt.Errorf("phase %s has no single artifact file", p)
	}
	var v T
	if err := readArtifact(filepath.Join(s.dir, name), &v); err != nil {
		return zero, err
	}
	if err := PT(&v).validate(); err != nil {
		return zero, err
	}
	return v, nil
}

func readArtifact(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact %s not written yet", filepath.Base(path))
		}
		return err
	}
	return json.Unmarshal(data, into)
}

// Position reports the first phase whose output is missing or invalid, or
// PhaseDone when the run is complete.
//
// Preview has no output of its own to check. It is evidenced by capture, because
// a screenshot cannot exist without a page having been served, so the two share
// the screenshots as their evidence and no caller has to special-case either.
func (s *Store) Position() (Phase, error) {
	for _, spec := range phases {
		ok, err := s.satisfied(spec)
		if err != nil {
			return "", err
		}
		if !ok {
			return spec.phase, nil
		}
	}
	return PhaseDone, nil
}

// satisfied reports whether a phase's output is present and valid. A phase that
// writes no single file still has to be answerable, which is why capture reads
// the screenshots and review reads the passes rather than consulting the table.
func (s *Store) satisfied(spec phaseSpec) (bool, error) {
	switch spec.phase {
	case PhaseReview:
		// A run is reviewed when the review that ends it is on disk. An
		// intermediate review is deliberately not enough: it is a request for
		// another pass, not a completed run.
		passes, err := s.Reviews()
		if err != nil {
			return false, err
		}
		if len(passes) == 0 {
			return false, nil
		}
		r, err := LoadReview(s, passes[len(passes)-1])
		if err != nil {
			return false, err
		}
		if !r.NeedsRepair() {
			return true, nil
		}
		// A repair verdict also ends the run once the budget is spent. Without
		// this the last review keeps reading "repair" forever, so a restarted
		// run is sent back to review with a fresh budget and overwrites the
		// early passes. The bound has to be part of the derived position, not
		// only of the loop that first reached it.
		return r.Pass >= MaxRepairs, nil
	case PhaseCapture, PhasePreview:
		// A screenshot is proof that a page was served, so it settles both.
		// Recording a preview separately would mean persisting a port number
		// that stopped being true the moment the process ended.
		return s.captured()
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, spec.artifact))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return validateJSON(spec.phase, spec.artifact, raw)
}

// LoadReview reads one review pass.
func LoadReview(s *Store, pass int) (Review, error) {
	var r Review
	if err := readArtifact(filepath.Join(s.dir, reviewFile(pass)), &r); err != nil {
		return r, err
	}
	if err := r.validate(); err != nil {
		return r, err
	}
	return r, nil
}

// Reviews returns the passes on disk, oldest first.
func (s *Store) Reviews() ([]int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var passes []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "review-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "review-"), ".json"))
		if err != nil {
			continue
		}
		passes = append(passes, n)
	}
	for i := 1; i < len(passes); i++ {
		for j := i; j > 0 && passes[j] < passes[j-1]; j-- {
			passes[j], passes[j-1] = passes[j-1], passes[j]
		}
	}
	return passes, nil
}

// Repairs reports how many repair passes a run has spent, which is one less
// than the number of reviews on disk. The count lives in the files rather than a
// counter because a repair deletes the artifacts after the one it rewinds to,
// and a counter that lived only in memory or in a single mutable file would be
// the first thing lost.
func (s *Store) Repairs() (int, error) {
	passes, err := s.Reviews()
	if err != nil {
		return 0, err
	}
	if len(passes) == 0 {
		return 0, nil
	}
	return len(passes) - 1, nil
}

// Drop deletes the artifacts of from and every phase after it, so a repair pass
// makes Position walk back on its own. Reviews are kept: they are the record of
// which pass asked for what, and the repair count is derived from them.
func (s *Store) Drop(from Phase) error {
	if !isPhase(from) {
		return fmt.Errorf("unknown phase %q", from)
	}
	names := []string{}
	dropping := false
	for _, spec := range phases {
		if spec.phase == from {
			dropping = true
		}
		if !dropping {
			continue
		}
		switch spec.phase {
		case PhaseReview, PhasePreview, PhaseCapture:
			continue
		}
		if spec.artifact != "" {
			names = append(names, spec.artifact)
		}
	}
	if from == PhaseCapture {
		names = append(names, shotFile(Desktop), shotFile(Mobile))
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func artifactName(p Phase) string {
	for _, spec := range phases {
		if spec.phase == p {
			return spec.artifact
		}
	}
	return ""
}

// writeFileAtomic writes through a temp file and renames, so a reader never
// sees a half-written artifact and a crash never leaves one that parses.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".design-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validateJSON checks an artifact already on disk against its phase's rules, so
// a hand-edited or partially written file cannot stand in for a finished phase.
func validateJSON(p Phase, name string, raw []byte) (bool, error) {
	switch p {
	case PhaseQualify:
		var v Qualify
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	case PhaseBrief:
		var v Brief
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	case PhaseBrand:
		var v Brand
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	case PhasePage:
		var v Page
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	case PhaseAssets:
		var v Assets
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	case PhaseBuild:
		var v Build
		err := json.Unmarshal(raw, &v)
		return err == nil && v.validate() == nil, nil
	}
	return false, fmt.Errorf("no validator for artifact %s", name)
}
