package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fuzzy matching over a bounded directory walk.
//
// A shell function that does `find ~/Projects -maxdepth 2 | fzf` is the fastest
// way to reach a project, and the reason a sidebar full of typed paths never
// caught up. This is that, without the shell.

// DefaultSearchRoot is where a search starts when the caller does not say.
//
// It is ~/Projects when that exists, because that is where work lives, and
// ~/ otherwise. Walking the whole home directory three deep returns npm caches,
// tool installs and dotfile repositories, none of which are somewhere you work.
// A shell function that does `cd ~/Projects && find . -maxdepth 2 | fzf` is the
// same answer, arrived at by someone who has to type their own projects.
func DefaultSearchRoot(home string) string {
	if home == "" {
		return "."
	}
	projects := filepath.Join(home, "Projects")
	if info, err := os.Stat(projects); err == nil && info.IsDir() {
		return projects
	}
	return home
}

// SearchDirs walks root up to depth and returns directories matching query,
// best first.
//
// An empty query returns everything, ordered by how recently it was used, so
// opening the picker lands on somewhere you have actually been rather than
// alphabetically.
func SearchDirs(root, query string, depth, limit int) ([]Completion, error) {
	if depth <= 0 {
		depth = 3
	}
	if limit <= 0 {
		limit = 100
	}
	abs, err := filepath.Abs(expandTilde(root, ""))
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, nil
	}

	var found []Completion
	// The root itself is a valid target: picking the home directory is a real
	// choice, not a mistake.
	if entry, err := os.Stat(abs); err == nil {
		found = append(found, Completion{
			Name: filepath.Base(abs), Path: abs, Dir: true,
			HasChildren: true, Mtime: entry.ModTime().UnixNano(),
		})
	}
	_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path == abs {
			return nil
		}
		// Hidden directories are skipped at every depth. A list of somewhere to
		// work should not open on .stfolder, .vscode or .ipynb_checkpoints,
		// which sort ahead of every real project because "." precedes a
		// letter. The exception that used to be here, a hidden directory
		// directly under the root, is what put them at the top.
		if strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil {
			return nil
		}
		if depthOf(rel) > depth {
			return filepath.SkipDir
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		// node_modules and build output are large, uninteresting, and slow to
		// walk. Skipping them is most of the speed.
		if skip := shouldSkipDir(d.Name()); skip {
			return filepath.SkipDir
		}
		found = append(found, Completion{
			Name: d.Name(), Path: path, Dir: true,
			HasChildren: true, Mtime: info.ModTime().UnixNano(),
		})
		return nil
	})
	// Rank against what was typed, shown relative to the root so the visible
	// text is short and the ranking is against something readable.
	var scored []scoredDir
	for _, c := range found {
		rel, relErr := filepath.Rel(abs, c.Path)
		if relErr != nil {
			continue
		}
		if rel == "." {
			continue
		}
		candidate := rel
		score, indices, ok := FuzzyMatchIndices(query, candidate)
		if !ok {
			continue
		}
		// The indices address the path relative to the root, which is the text a
		// caller shows, so they line up with what is on screen.
		c.Match = indices
		// A recent directory outranks an old one at equal match quality, which
		// is what makes an empty query land somewhere useful.
		scored = append(scored, scoredDir{Completion: c, label: candidate, score: score, mtime: c.Mtime})
	}
	if query == "" {
		// Nothing typed means nothing to disambiguate, so the list is simply
		// what is there, in order. Ranking by how recently a directory was
		// touched answers a question nobody asked: it put whichever project
		// happened to be edited most recently at the top, which is a fact about
		// the last hour rather than about where someone works.
		sort.Slice(scored, func(i, j int) bool { return scored[i].label < scored[j].label })
	} else {
		// With something typed the question is which one was meant. A directory
		// that looks like a project outranks one that does, whatever the match
		// quality: "eng" should not rank a folder in Library above your code.
		for i := range scored {
			if looksLikeProject(scored[i].Path) {
				scored[i].score += 200
			}
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].score != scored[j].score {
				return scored[i].score > scored[j].score
			}
			return scored[i].mtime > scored[j].mtime
		})
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]Completion, 0, len(scored))
	for _, s := range scored {
		// The row shows a name and a path. Repeating the name at the end of an
		// absolute path is noise, so the path is relative to the root searched.
		item := s.Completion
		item.Name = s.label
		out = append(out, item)
	}
	return out, nil
}

type scoredDir struct {
	Completion
	label string
	score int
	mtime int64
}

func depthOf(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func shouldSkipDir(name string) bool {
	switch name {
	// Build output and dependencies: large, uninteresting, and slow to walk.
	case "node_modules", ".git", "build", "dist", "target", "vendor",
		"DerivedData", ".next", ".venv", "__pycache__":
		return true
	// macOS keeps a great deal of state under the home directory that is
	// nobody's project. An empty search that returns PassKit, Keychains and
	// com.apple.cache_delete is not a list of somewhere to work.
	case "Library", "Applications", "Movies", "Music", "Pictures", "Public",
		"Applications (Parallels)", "Parallels", "Trash", "go/pkg", "Library/Application Support":
		return true
	}
	return false
}

// projectMarkers are the files whose presence means a directory is something
// you work in rather than something that merely exists.
var projectMarkers = []string{
	".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml",
	"Makefile", "CMakeLists.txt", "pom.xml", "build.gradle", "Gemfile", "composer.json",
}

// looksLikeProject reports whether a directory carries a marker file, or is one
// level below a directory that does.
//
// This is what makes an empty query land somewhere useful. Ranking purely by
// recency returns whatever was touched last, which on a Mac is usually a
// system directory that happened to be written to.
func looksLikeProject(path string) bool {
	if hasMarker(path) {
		return true
	}
	// A monorepo: escape/engine under escape/. One level down is enough to
	// catch that without dragging in everything.
	parent := filepath.Dir(path)
	if parent == path {
		return false
	}
	if !strings.HasPrefix(parent, "/") {
		if hasMarker(parent) {
			return true
		}
	}
	return false
}

func hasMarker(dir string) bool {
	for _, marker := range projectMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// FuzzyMatch reports whether query matches candidate as a subsequence, and
// scores it. Higher is better.
//
// The scoring rewards the things that make a match feel right: matching the
// folder name rather than a parent, matching at the start of a segment, runs of
// consecutive characters, and a short path. That ordering is what puts
// "engine" above "Projects/engineering-notes" when you type "eng".
func FuzzyMatch(query, candidate string) (int, bool) {
	score, _, ok := FuzzyMatchIndices(query, candidate)
	return score, ok
}

// FuzzyMatchIndices is FuzzyMatch, and it also reports which characters of the
// candidate the query matched.
//
// The indices exist so a caller can highlight what matched without writing a
// second matcher. A client-side greedy pass would highlight different characters
// from the ones that produced the order, and a fuzzy finder that marks letters
// the ranker never used is worse than one that marks none: it explains a
// ranking it did not produce.
func FuzzyMatchIndices(query, candidate string) (int, []int, bool) {
	if query == "" {
		return 1, nil, true
	}
	lowerQuery := strings.ToLower(query)
	lowerCandidate := strings.ToLower(candidate)
	segments := strings.Split(lowerCandidate, string(filepath.Separator))
	// The folder name is the most likely thing being typed.
	name := segments[len(segments)-1]

	score := 0
	qi := 0
	matchedInName := false
	var indices []int
	for si, segment := range segments {
		start := 0
		// Where this segment starts in the whole candidate, so the indices
		// returned address the string the caller actually has.
		base := 0
		for i := 0; i < si; i++ {
			base += len(segments[i]) + 1
		}
		for qi < len(lowerQuery) && start < len(segment) {
			if segment[start] == lowerQuery[qi] {
				indices = append(indices, base+start)
				qi++
			}
			start++
		}
		if start > 0 && si == len(segments)-1 {
			matchedInName = true
		}
	}
	if qi < len(lowerQuery) {
		return 0, nil, false
	}

	if matchedInName {
		score += 60
	} else {
		score += 20
	}
	if strings.HasPrefix(name, lowerQuery) {
		score += 50
	} else if strings.Contains(name, lowerQuery) {
		score += 30
	}
	// Consecutive runs, which is what separates a real match from scattered
	// characters that happen to appear in order.
	runs := countRuns(lowerQuery, lowerCandidate)
	score += runs * 8
	// Shorter paths win ties, so a top-level folder beats a deeply nested one.
	score -= len(segments) * 4
	return score, indices, true
}

func countRuns(query, candidate string) int {
	runs, qi, inRun := 0, 0, false
	for i := 0; i < len(candidate) && qi < len(query); i++ {
		if candidate[i] == query[qi] {
			if !inRun {
				runs++
				inRun = true
			}
			qi++
			continue
		}
		inRun = false
	}
	return runs
}
