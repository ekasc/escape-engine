package design

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Tool is an allowlisted way to serve a built page. The set is closed on
// purpose: a preview runner that accepted a command string would be the generic
// bash tool with a narrower name, and what reaches it can be model output.
type Tool string

const (
	// ToolStatic serves a directory of files from inside the engine. It is the
	// right tool for a built page, which is files, and it has no external
	// dependency that can be missing, broken, or a different version than
	// expected. This machine's python3 is a dangling symlink, which is exactly
	// why it is not a dependency.
	ToolStatic Tool = "static"
	// ToolNPM runs the project's own dev script, for a project that needs a real
	// dev server rather than a directory of files.
	ToolNPM Tool = "npm"
)

// npmRecipe is the only recipe that starts a process. Every argv a dev server is
// ever run with is written here, so adding a tool is a table edit rather than a
// new path through the runner.
var npmRecipe = struct {
	bin  string
	args func(port int) []string
	env  func(port int) []string
}{
	bin:  "npm",
	args: func(port int) []string { return []string{"run", "dev", "--", "--port", strconv.Itoa(port)} },
	env:  func(port int) []string { return []string{"PORT=" + strconv.Itoa(port)} },
}

// readyTimeout bounds how long a server may take to answer before the start is
// treated as failed.
const readyTimeout = 30 * time.Second

// stopGrace is how long a preview server gets to exit on SIGINT before it is
// killed. A dev server with a watcher usually needs a moment to unwind.
const stopGrace = 3 * time.Second

// Preview is a running local server. It is a handle, not a session: a caller
// cannot drive the server's lifecycle in pieces, only start it and stop it.
type Preview struct {
	url string
	// cmd is set for a child dev server and srv for the in-process static
	// server. Exactly one is ever set.
	cmd *exec.Cmd
	srv *http.Server
	// done is closed once the process has been reaped and waitErr holds why.
	// Closing a channel rather than sending on a buffered one is what lets Stop
	// be called more than once: a second waiter would block forever on a value
	// channel nobody is going to send to again.
	done    chan struct{}
	waitErr error
	// log collects the server's output, which is the only diagnosis available
	// when readiness times out.
	log     *os.File
	stopped sync.Once
	stopErr error
}

// StartPreview serves dir with an allowlisted tool and returns once the server
// answers. dir must resolve inside root.
func StartPreview(ctx context.Context, root, dir string, tool Tool) (*Preview, error) {
	abs, err := confine(root, dir)
	if err != nil {
		return nil, fmt.Errorf("preview directory: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	p := &Preview{url: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}

	switch tool {
	case ToolStatic:
		p.srv = &http.Server{
			Handler: http.FileServer(http.Dir(abs)),
			// Loopback only. A preview of the user's own build has no reason to
			// answer anything that is not on this machine.
			Addr:              p.url[len("http://"):],
			ReadHeaderTimeout: 10 * time.Second,
		}
		listener, err := net.Listen("tcp", p.url[len("http://"):])
		if err != nil {
			return nil, err
		}
		p.done = make(chan struct{})
		go func() {
			p.waitErr = p.srv.Serve(listener)
			close(p.done)
		}()
	case ToolNPM:
		if err := p.startChild(abs, port); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("no preview recipe for %q", tool)
	}

	if err := p.await(ctx); err != nil {
		p.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("%s: %w%s", tool, err, p.logTail())
	}
	return p, nil
}

// startChild runs the project's own dev server. It gets its own process group so
// a Stop can signal the whole tree: a dev server that spawns a bundler child
// would otherwise leave that child holding the port after the parent was gone.
func (p *Preview) startChild(dir string, port int) error {
	if _, err := exec.LookPath(npmRecipe.bin); err != nil {
		return fmt.Errorf("preview tool %s: %w", npmRecipe.bin, err)
	}
	// The log lives outside the served directory on purpose. Writing it inside
	// would put the server's own output on the port the capture photographs.
	logFile, err := os.CreateTemp("", "design-preview-*.log")
	if err != nil {
		return err
	}
	cmd := exec.Command(npmRecipe.bin, npmRecipe.args(port)...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), npmRecipe.env(port)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.Remove(logFile.Name())
		return fmt.Errorf("start %s: %w", npmRecipe.bin, err)
	}

	p.cmd = cmd
	p.log = logFile
	// Exactly one Wait, started once, so Stop and the readiness check cannot
	// both reap the process.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return nil
}

// URL is the address to capture or open. It is only meaningful after StartPreview
// has returned, which is the point at which the server answered.
func (p *Preview) URL() string { return p.url }

// Stop ends the server and everything it started. It is safe to call more than
// once, and safe to call on a context that has already been cancelled, because
// a failed capture is exactly when the server most needs shutting down.
func (p *Preview) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.stopped.Do(func() { p.stopErr = p.terminate(ctx) })
	return p.stopErr
}

// terminate signals the server's process group, so a dev server that spawned a
// bundler child does not leave that child holding the port. Signalling a pid
// that has already been reaped would hit whatever process inherited the number,
// which is why this runs at most once.
func (p *Preview) terminate(ctx context.Context) error {
	if p.srv != nil {
		p.srv.Close()
		<-p.done
		return nil
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	pgid := -p.cmd.Process.Pid
	syscall.Kill(pgid, syscall.SIGINT)

	select {
	case <-p.done:
		return p.discardLog()
	case <-time.After(stopGrace):
		syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done
		return p.discardLog()
	case <-ctx.Done():
		syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done
		p.discardLog()
		return ctx.Err()
	}
}

// discardLog closes the server's output and removes it. The log is a diagnostic
// for this process, not an artifact a run keeps.
func (p *Preview) discardLog() error {
	if p.log == nil {
		return nil
	}
	name := p.log.Name()
	p.log.Close()
	p.log = nil
	os.Remove(name)
	return nil
}

// await blocks until the server answers an HTTP request or gives up.
func (p *Preview) await(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			// The server is gone, so there is nothing left to wait for. Report
			// why rather than timing out on a process that already exited.
			return fmt.Errorf("exited before becoming ready: %v", p.waitErr)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(p.url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("not ready after %s", readyTimeout)
}

// logTail returns the tail of the server's output, so a start failure says what
// the server said rather than only that it was silent. It is read before the log
// is discarded.
func (p *Preview) logTail() string {
	if p.log == nil {
		return ""
	}
	data, err := os.ReadFile(p.log.Name())
	if err != nil || len(data) == 0 {
		return ""
	}
	const max = 800
	tail := data
	if len(tail) > max {
		tail = tail[len(tail)-max:]
	}
	return "\n" + string(tail)
}

// freePort asks the kernel for an unused port and hands the number on. There is
// a window between the probe and the server binding it, which is why a failed
// bind surfaces as a readiness failure with the server's own log rather than as
// a silent hang.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
