package session

import "fmt"

// UndoPath creates a new session ending immediately before the latest user turn.
func UndoPath(srcPath, cwd string, roots ...string) (string, error) {
	root := DefaultRoot()
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	entries, err := ReadAll(srcPath)
	if err != nil {
		return "", err
	}
	lastUser := -1
	for i, entry := range entries {
		if entry.Type == TypeMessage && entry.Message != nil && entry.Message.Role == RoleUser {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return "", fmt.Errorf("no user turn to undo")
	}
	parent := ""
	for i := lastUser - 1; i >= 0; i-- {
		if entries[i].Type != TypeSession {
			parent = entries[i].ID
			break
		}
	}
	if parent == "" {
		return NewPath(root, cwd)
	}
	newPath, _, err := Fork(srcPath, parent, cwd)
	return newPath, err
}
