// Package rpc implements the stdio JSON-RPC 2.0 control surface that Babylon
// talks to: requests arrive as JSON-Lines on stdin, responses and agent
// events stream as JSON-Lines on stdout.
//
// Wire layout (one JSON object per line on each side):
//
//	request:  {"jsonrpc":"2.0","id":1,"method":"send","params":{"text":"..."}}
//	response: {"jsonrpc":"2.0","id":1,"result":{...}}
//	event:    {"type":"event","event":"agent_settled","reason":"done",...}
//
// Responses and events are distinguishable: responses carry "jsonrpc" and
// the request id; events carry "type":"event".
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ekasc/pi-go/internal/agent"
	"github.com/ekasc/pi-go/internal/session"
)

// JSON-RPC error codes.
const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeNotFound = -32601
	codeInternal = -32603
)

// Server is the JSON-RPC dispatcher over an agent.
type Server struct {
	agent *agent.Agent
	root  string // sessions root for list-sessions
	cwd   string
	store *session.Store

	outMu sync.Mutex
	out   io.Writer
	wg    sync.WaitGroup // in-flight dispatches
}

// NewServer builds a server bound to an agent and an output writer.
func NewServer(a *agent.Agent, store *session.Store, root, cwd string, out io.Writer) *Server {
	return &Server{agent: a, store: store, root: root, cwd: cwd, out: out}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(append(b, '\n'))
}

// Relay forwards agent events to the output as {"type":"event",...} lines.
// The session_started event is written synchronously by Serve. Blocks until
// ctx is done.
func (s *Server) Relay(ctx context.Context, events <-chan agent.Event) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.write(ev)
		case <-ctx.Done():
			return
		}
	}
}

// Serve reads requests from in until EOF (or ctx cancel) and dispatches each
// in its own goroutine. When stdin closes — the parent's way of saying it's
// done — the current turn is stopped and in-flight dispatches are drained
// before returning, so responses and settle events are not lost.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	// The session header event goes out first, synchronously, so clients
	// always see session_started before any response or turn event.
	s.write(agent.Event{
		Type:        "event",
		Event:       agent.EventSessionStarted,
		SessionID:   s.store.ID(),
		SessionFile: s.store.Path(),
		Cwd:         s.cwd,
	})

	sc := newScanner(in)
	for {
		line, ok := sc.Next()
		if !ok {
			break
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, codeParse, "parse error: "+err.Error())
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.dispatch(ctx, req)
		}()
	}
	// Drain: stop any running turn and give dispatches a moment to respond.
	s.agent.Stop()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req request) {
	if req.JSONRPC != "2.0" {
		s.writeError(req.ID, codeInvalid, "invalid request: jsonrpc must be 2.0")
		return
	}
	switch req.Method {
	case "send":
		var p struct {
			Text string `json:"text"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			s.writeError(req.ID, codeInvalid, "invalid params: "+err.Error())
			return
		}
		turnID, err := s.agent.Send(p.Text)
		if err != nil {
			s.writeError(req.ID, codeInternal, err.Error())
			return
		}
		s.writeResult(req.ID, map[string]any{"turnId": turnID})

	case "steer":
		var p struct {
			Text string `json:"text"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			s.writeError(req.ID, codeInvalid, "invalid params: "+err.Error())
			return
		}
		turnID, err := s.agent.Steer(p.Text)
		if err != nil {
			s.writeError(req.ID, codeInternal, err.Error())
			return
		}
		s.writeResult(req.ID, map[string]any{"turnId": turnID})

	case "stop":
		s.agent.Stop()
		s.writeResult(req.ID, map[string]any{})

	case "state":
		s.writeResult(req.ID, s.agent.State())

	case "recap":
		text, err := s.agent.Recap(ctx)
		if err != nil {
			s.writeError(req.ID, codeInternal, err.Error())
			return
		}
		s.writeResult(req.ID, map[string]any{"text": text})

	case "list-sessions":
		var p struct {
			Cwd string `json:"cwd"`
		}
		_ = decodeParams(req.Params, &p)
		root := s.root
		infos, err := session.List(root)
		if err != nil {
			s.writeError(req.ID, codeInternal, err.Error())
			return
		}
		if p.Cwd != "" {
			filtered := infos[:0]
			for _, i := range infos {
				if i.Cwd == p.Cwd {
					filtered = append(filtered, i)
				}
			}
			infos = filtered
		}
		s.writeResult(req.ID, map[string]any{"sessions": infos})

	case "ping":
		s.writeResult(req.ID, map[string]any{"pong": true})

	default:
		s.writeError(req.ID, codeNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) writeResult(id json.RawMessage, result any) {
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func decodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
