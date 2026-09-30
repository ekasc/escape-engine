package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Completion is one candidate for a partially typed path.
type Completion struct {
	// Name is the final path segment, which is what the field shows.
	Name string `json:"name"`
	// Path is the absolute path to insert.
	Path string `json:"path"`
	// Dir marks a directory, so a caller completing to a directory can stop here.
	Dir bool `json:"dir"`
	// HasChildren means the directory is not empty, so completing into it is
	// worth offering.
	HasChildren bool `json:"hasChildren"`
	// Match holds the indices in the path relative to the search root that the
	// query matched, so a caller can highlight what matched without
	// reimplementing the matcher. Nil when the result did not come from a fuzzy
	// query.
	Match []int `json:"match,omitempty"`
	// Mtime is when the directory changed, used to rank equally good matches.
	Mtime int64 `json:"mtime,omitempty"`
}

// MaxCompletions bounds a reply. A directory with ten thousand entries would
// otherwise send all of them to decide between two matches.
const MaxCompletions = 200

// CompletePath returns directory candidates for a partially typed path.
//
// Completion is only useful if it handles the way people actually type: a
// leading tilde, a trailing separator meaning "show me what is in here", and a
// fragment that matches nothing yet. It also refuses to walk somewhere that is
// not a directory, because completing into /etc/rc.d/a/b/c and then failing to
// read it helps nobody.
//
// Only directories are returned. A project is a directory, and offering files
// would fill the list with things that cannot be added.
func CompletePath(input string, home string) ([]Completion, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		// Nothing typed: offering the home directory is the useful first step.
		if home == "" {
			return nil, nil
		}
		return []Completion{{Name: "~", Path: home, Dir: true, HasChildren: true}}, nil
	}

	expanded := expandTilde(raw, home)
	// Typing a separator means "inside this", so the fragment is empty and the
	// parent is the typed path itself.
	endsWithSep := strings.HasSuffix(raw, string(filepath.Separator))
	dir, fragment := splitLast(expanded, endsWithSep)
	if dir == "" {
		dir = string(filepath.Separator)
	}

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	out := make([]Completion, 0, 16)
	for _, e := range entries {
		if !e.IsDir() {
			// A project is a directory. Offering files would fill the list with
			// things that cannot be added.
			continue
		}
		if strings.HasPrefix(e.Name(), ".") && fragment == "" {
			// Hidden entries are noise until asked for by name.
			continue
		}
		if fragment != "" && !strings.HasPrefix(e.Name(), fragment) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		c := Completion{Name: e.Name(), Path: full, Dir: e.IsDir()}
		if e.IsDir() {
			// Read one entry rather than the whole directory: the question is
			// whether there is anything to complete into, not what it holds.
			if children, readErr := os.ReadDir(full); readErr == nil {
				c.HasChildren = len(children) > 0
			}
		}
		out = append(out, c)
		if len(out) >= MaxCompletions {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func expandTilde(path, home string) string {
	if home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// splitLast separates a path into the directory to read and the fragment being
// typed. A trailing separator means the whole thing is the directory.
func splitLast(path string, endsWithSep bool) (dir, fragment string) {
	if endsWithSep {
		return path, ""
	}
	i := strings.LastIndex(path, string(filepath.Separator))
	if i < 0 {
		return "", path
	}
	if i == 0 {
		return string(filepath.Separator), path[1:]
	}
	return path[:i], path[i+1:]
}

// CommonPrefix is the longest prefix every candidate shares. Inserting it lets
// a second Tab narrow a list rather than repeating the same completion.
func CommonPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	prefix := paths[0]
	for _, p := range paths[1:] {
		n := 0
		for n < len(prefix) && n < len(p) && prefix[n] == p[n] {
			n++
		}
		prefix = prefix[:n]
		if prefix == "" {
			return ""
		}
	}
	return prefix
}
