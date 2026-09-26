package session

import (
	"fmt"
	"os"
	"path/filepath"
)

// PruneResult reports what a prune of empty project directories would do.
type PruneResult struct {
	Scanned int
	Empty   []string
	Freed   int64
}

// PruneEmptyProjectDirs finds project directories under root that hold no
// session files.
//
// The store keeps a directory per project it has ever been opened for, and a
// directory that never received a session is only an entry. They are not free:
// every listing walks the whole root, so thousands of empty directories cost
// thousands of syscalls on the sidebar's first paint and on Settings.
//
// A directory is only reported when it contains no .jsonl file, so nothing a
// conversation depends on is ever a candidate. Subdirectories that are not
// themselves session roots are left alone.
func PruneEmptyProjectDirs(root string) (PruneResult, error) {
	res := PruneResult{}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		res.Scanned++
		path := filepath.Join(root, entry.Name())
		files, err := os.ReadDir(path)
		if err != nil {
			// Unreadable is not empty, and must never be removed.
			continue
		}
		hasSession := false
		other := int64(0)
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			if filepath.Ext(file.Name()) == ".jsonl" {
				hasSession = true
				break
			}
			if info, err := file.Info(); err == nil {
				other += info.Size()
			}
		}
		if hasSession || other > 0 {
			continue
		}
		res.Empty = append(res.Empty, entry.Name())
	}
	return res, nil
}

// PruneEmptyProjectDirsApply removes the directories PruneEmptyProjectDirs
// reported. It refuses a directory that has become non-empty since the scan,
// because between the two calls something may have been written there.
func PruneEmptyProjectDirsApply(root string) (removed int, err error) {
	found, err := PruneEmptyProjectDirs(root)
	if err != nil {
		return 0, err
	}
	for _, name := range found.Empty {
		path := filepath.Join(root, name)
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			continue
		}
		if len(entries) > 0 {
			// Something appeared after the scan. Leave it alone.
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil {
			continue
		}
		removed++
	}
	// Any removal changes what a scan would report, and leaves dead entries
	// behind in the index.
	InvalidateScan()
	if removed > 0 {
		DropIndex(root)
	}
	return removed, nil
}

// DescribePrune renders a prune result for a person to read before deciding.
func DescribePrune(res PruneResult) string {
	if len(res.Empty) == 0 {
		return fmt.Sprintf("scanned %d project directories, none are empty", res.Scanned)
	}
	return fmt.Sprintf(
		"scanned %d project directories, %d hold no sessions and would be removed",
		res.Scanned, len(res.Empty),
	)
}
