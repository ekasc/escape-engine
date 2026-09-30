package design

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// confine resolves rel against root and refuses anything that lands outside it.
//
// Both halves matter and neither is sufficient alone. Cleaning catches "..", and
// the prefix check catches absolute paths, but a symlink inside the workspace
// pointing at /etc passes both. So the candidate is resolved a second time
// through the real filesystem and re-checked, which is the step that actually
// closes the escape.
func confine(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is absolute, want relative to the workspace", rel)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace root: %w", err)
	}
	// Clamping an escape into the root would turn ".." into a valid request for
	// a different directory, which is worse than refusing it: the caller asked
	// for something outside and would be served something inside without being
	// told. An explicit climb is an error.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q climbs above the workspace", rel)
	}
	joined := filepath.Join(realRoot, cleaned)
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// A path that does not exist yet is legitimate: the build pass creates
		// files. Resolve the deepest existing ancestor and confine that, so a
		// new file under a symlinked directory is still checked.
		parent, base := filepath.Split(joined)
		realParent, perr := filepath.EvalSymlinks(filepath.Clean(parent))
		if perr != nil {
			return "", fmt.Errorf("resolve %q: %w", rel, err)
		}
		real = filepath.Join(realParent, base)
	}
	if !within(realRoot, real) {
		return "", fmt.Errorf("path %q resolves outside the workspace", rel)
	}
	return real, nil
}

// within reports whether path is root or lies under it.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// validSessionID rejects anything that is not a single safe path segment. A
// session id becomes a directory name, so a separator or a ".." in one would
// relocate a run's artifacts outside the store.
func validSessionID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	return true
}
