package session

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// scanTTL bounds how long a header scan is reused. Every caller in one
// interaction needs the same walk of every project directory, and that walk is
// the single most expensive thing Escape does at rest. Thirty seconds is long
// enough to collapse a launch plus a settings visit into one scan, and short
// enough that a stale sidebar is not something a person notices.
//
// A write to a session invalidates it immediately, so Escape never lists its own
// new work as missing. Another process writing into the same store is visible
// within the TTL.
const scanTTL = 30 * time.Second

var (
	scanMu      sync.Mutex
	scanRoot    string
	scanAt      time.Time
	scanHeaders []header
)

// InvalidateScan drops the cached header scan. Called whenever Escape creates or
// appends to a session.
func InvalidateScan() {
	scanMu.Lock()
	scanAt = time.Time{}
	scanMu.Unlock()
}

func cachedHeaders(root string) ([]header, error) {
	scanMu.Lock()
	defer scanMu.Unlock()
	now := time.Now()
	if scanRoot == root && !scanAt.IsZero() && now.Sub(scanAt) < scanTTL {
		return scanHeaders, nil
	}
	found, err := walkHeaders(root, func(string) bool { return true })
	if err != nil {
		// A failed scan must not poison the cache with an empty result.
		return nil, err
	}
	scanRoot, scanAt, scanHeaders = root, now, found
	return found, nil
}

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

type header struct {
	path string
	cwd  string
}

// walkHeaders probes every session header under root concurrently and returns
// the ones whose cwd passes keep, in directory order.
//
// The cost here is filesystem metadata, so both phases run across a bounded
// worker pool and each result lands in its own slot, leaving the caller's sort to
// decide the order. The worker count is capped because the bottleneck is
// syscalls, and oversubscribing them makes every probe slower.
//
// The directory name is deliberately not trusted. A session's directory is
// derived from an unresolved path while its header records a resolved one, and on
// macOS /var and /private/var name the same directory differently, so a
// directory filter would silently drop real sessions.
func walkHeaders(root string, keep func(cwd string) bool) ([]header, error) {
	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	perDir := make([][]header, len(dirs))
	workers := runtime.NumCPU() * 2
	if workers < 4 {
		workers = 4
	}
	if workers > 16 {
		workers = 16
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(dirs) {
					return
				}
				dir := dirs[i]
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
					cwd, ok := ReadCwd(path)
					if !ok {
						continue
					}
					perDir[i] = append(perDir[i], header{path: path, cwd: cwd})
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, found := range perDir {
		total += len(found)
	}
	all := make([]header, 0, total)
	for _, found := range perDir {
		all = append(all, found...)
	}
	out := make([]header, 0, len(all))
	for _, h := range all {
		if keep(h.cwd) {
			out = append(out, h)
		}
	}
	return out, nil
}

// ListForCwd returns this project's sessions, newest first.
//
// List reads a bounded window from both ends of every session in the store,
// which is the right cost for "show me everything" and badly wrong for "show me
// this project". A store with thousands of sessions spends seconds on files
// whose header already says they belong elsewhere. So this probes each header
// and only pays the full read for files that match.
func ListForCwd(root, cwd string) ([]Info, error) {
	want := filepath.Clean(cwd)
	all, err := cachedHeaders(root)
	if err != nil {
		return nil, err
	}
	matches := make([]header, 0, 16)
	for _, h := range all {
		if filepath.Clean(h.cwd) == want {
			matches = append(matches, h)
		}
	}
	defer FlushIndex(root)
	infos := make([]Info, 0, 16)
	for _, m := range matches {
		info, err := indexInfo(root, m.path)
		if err != nil || info == nil {
			continue
		}
		infos = append(infos, *info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Mtime > infos[j].Mtime })
	return infos, nil
}

// Projects returns the distinct working directories that hold sessions, most
// recently touched first.
//
// It is built on the same header probe rather than on List, because knowing which
// projects exist should never cost what reading every session costs.
func Projects(root string, limit int) ([]string, error) {
	found, err := cachedHeaders(root)
	if err != nil {
		return nil, err
	}
	newest := make(map[string]int64, 16)
	order := make([]string, 0, 16)
	for _, h := range found {
		abs, err := filepath.Abs(h.cwd)
		if err != nil || !filepath.IsAbs(abs) {
			// A relative cwd is not somewhere anyone works.
			continue
		}
		clean := filepath.Clean(abs)
		if clean == string(filepath.Separator) {
			continue
		}
		if _, seen := newest[clean]; !seen {
			order = append(order, clean)
		}
		stat, err := os.Stat(h.path)
		if err == nil {
			if m := stat.ModTime().UnixMilli(); m > newest[clean] {
				newest[clean] = m
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return newest[order[i]] > newest[order[j]] })
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	return order, nil
}
