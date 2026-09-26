package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// headerProbeBytes is how much of a session file is read to reject it cheaply.
// The session header is the first line and carries the cwd, so a probe only has
// to be large enough to hold it. parseLines silently drops a truncated final
// line, so a short read cannot invent a partial entry.
//
// 2 KB against a measured maximum header of 557 bytes over 300 sampled
// sessions. A header larger than this would be skipped as unreadable, which is
// the same outcome as a corrupt file rather than a silent wrong answer.
const headerProbeBytes = 2 * 1024

// ReadCwd returns the cwd recorded in a session file's header without paying for
// the bounded head+tail window ReadInfo needs. ok is false when the file has no
// readable session header, which is the same condition under which ReadInfo
// returns nil.
func ReadCwd(path string) (cwd string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	head := make([]byte, headerProbeBytes)
	n, err := f.Read(head)
	if err != nil && n == 0 {
		return "", false
	}
	for _, obj := range parseLines(head[:n]) {
		e, isEntry := obj.(Entry)
		if isEntry && e.Type == TypeSession && e.ID != "" {
			return e.Cwd, true
		}
	}
	return "", false
}

// ListForCwd returns this project's sessions, newest first.
//
// List reads a bounded window from both ends of every session in the store,
// which is the right cost for "show me everything" and badly wrong for "show me
// this project". A store with thousands of sessions spends seconds on files
// whose header already says they belong elsewhere. So this probes each header
// first and only pays the full read for files that match.
//
// The filter is the same header field and the same comparison List's callers
// apply, so the result is identical to filtering List's output. The directory
// name is deliberately not trusted: a session file's directory is derived from
// an unresolved path while its header records a resolved one, so /var and
// /private/var disagree for the same directory.
func ListForCwd(root, cwd string) ([]Info, error) {
	want := filepath.Clean(cwd)
	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, 16)
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(root, dir.Name(), file.Name())
			fileCwd, ok := ReadCwd(path)
			if !ok || filepath.Clean(fileCwd) != want {
				continue
			}
			info, err := ReadInfo(path)
			if err != nil || info == nil {
				continue
			}
			infos = append(infos, *info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Mtime > infos[j].Mtime })
	return infos, nil
}
