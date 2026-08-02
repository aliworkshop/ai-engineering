package web

import (
	"context"
	"errors"
	"sync"
	"time"
)

// approvalTimeout is how long a gated tool waits for the human to click before
// giving up. It denies on expiry: an unanswered question is not a yes, and the
// alternative is a turn wedged forever because someone closed the tab.
const approvalTimeout = 5 * time.Minute

// errNoStream means nothing is listening — there is no turn in flight, or the
// browser hung up mid-turn.
var errNoStream = errors.New("web: no live turn to report to")

// Session is one browser's conversation, and the human on the other end of it.
// It plays the role ui.Console plays in the terminal: it satisfies
// tools.Approver, so a dangerous tool asks *it* for the y/n, and it carries the
// hooks the agent reports tool calls and compaction through.
type Session struct {
	id        string
	assistant Assistant

	// turn admits one question at a time. The agent rewrites its history as it
	// works, so turns must not overlap.
	turn sync.Mutex

	// stream and ctx belong to the turn currently running. Ask calls every hook
	// — LogTool, LogCompact, Confirm — synchronously on the goroutine that set
	// these, so they are only ever touched by that one goroutine and need no
	// lock of their own.
	stream *sseWriter
	ctx    context.Context

	// pending is the one piece of session state a *different* request goroutine
	// reaches: the approval reply arrives on its own POST while the turn sits
	// blocked inside Confirm. Hence its own mutex.
	pmu     sync.Mutex
	pending map[string]chan bool

	// lastSeen is guarded by Server.mu, not by anything here.
	lastSeen time.Time
}

// newSession starts a conversation. The assistant is built last because the
// factory needs the session itself — that is what the agent's tools will ask
// for approval.
func newSession(newAssistant NewAssistant) *Session {
	s := &Session{
		id:       newID(),
		pending:  make(map[string]chan bool),
		lastSeen: time.Now(),
	}
	s.assistant = newAssistant(s)
	return s
}

// begin and end mark the turn a stream belongs to. Between them, everything the
// agent reports goes to this browser; outside them there is nowhere to report
// to, and emit says so.
func (s *Session) begin(ctx context.Context, stream *sseWriter) {
	s.ctx = ctx
	s.stream = stream
}

func (s *Session) end() {
	s.stream = nil
	s.ctx = nil
}

// emit sends one event to the browser watching this turn.
func (s *Session) emit(e event) error {
	if s.stream == nil {
		return errNoStream
	}
	return s.stream.send(e)
}

// Confirm implements tools.Approver. It shows the pending action in the page
// and blocks the tool until the human clicks Approve or Deny — the same
// contract the terminal's y/n prompt has, with the answer arriving on a
// separate request instead of on stdin.
//
// It denies whenever it can't get a real yes: no live turn, a browser that hung
// up, or nobody home for approvalTimeout.
func (s *Session) Confirm(action string) bool {
	// Register before asking, so a decision that comes back immediately still
	// finds somewhere to land. The channel is buffered, so resolve never blocks
	// on a Confirm that has already given up.
	reply := make(chan bool, 1)
	id := newID()

	s.pmu.Lock()
	s.pending[id] = reply
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pending, id)
		s.pmu.Unlock()
	}()

	if err := s.emit(event{Type: "approval", ID: id, Action: action}); err != nil {
		return false
	}

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case approved := <-reply:
		return approved
	case <-ctx.Done():
		return false // the browser went away mid-question
	case <-time.After(approvalTimeout):
		return false
	}
}

// resolve hands a decision to the Confirm that is waiting for it. It reports
// false if that approval is no longer pending — a stale click, or a turn that
// ended while the card was still on screen.
func (s *Session) resolve(id string, approved bool) bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()

	reply, ok := s.pending[id]
	if !ok {
		return false
	}
	delete(s.pending, id)
	reply <- approved
	return true
}

// LogTool and LogCompact are the agent's progress hooks. They mirror the two
// lines the console prints mid-turn; here they become events the page renders
// as it goes, so a long turn shows its work instead of sitting silent.
func (s *Session) LogTool(name, args, result string) {
	_ = s.emit(event{
		Type:    "tool",
		Name:    name,
		Args:    args,
		Result:  result,
		Diagram: redrawsCanvas(name),
	})
}

func (s *Session) LogCompact(summary string) {
	_ = s.emit(event{Type: "compact", Text: summary})
}

// redrawsCanvas reports whether a tool rewrites canvas.svg, which tells the
// page to reload the image it is showing. Naming the tools here — rather than
// having the page guess from the result text — keeps the trigger exact.
func redrawsCanvas(tool string) bool {
	switch tool {
	case "generate_diagram", "add_elements", "update_elements", "remove_elements":
		return true
	}
	return false
}
