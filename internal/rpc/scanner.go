package rpc

import (
	"bufio"
)

// lineScanner is a buffered line reader with a generous limit (steer text
// and tool args can be long).
type lineScanner struct {
	sc *bufio.Scanner
}

func newScanner(in interface{ Read([]byte) (int, error) }) *lineScanner {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &lineScanner{sc: sc}
}

// Next returns the next line (without the trailing newline).
func (l *lineScanner) Next() ([]byte, bool) {
	if !l.sc.Scan() {
		return nil, false
	}
	line := make([]byte, len(l.sc.Bytes()))
	copy(line, l.sc.Bytes())
	return line, true
}
