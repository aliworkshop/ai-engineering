package web

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
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

	// run holds what the turn currently in flight is doing. Like stream and
	// ctx it belongs to the turn's goroutine, except workflow, which a later
	// request reads to resume — hence the mutex on that one alone.
	crashAfter int    // cancel the turn after this many tools complete; 0 = never
	completed  int    // tools finished so far this turn
	crash      func() // cancels the turn, standing in for the process dying

	wmu      sync.Mutex
	workflow string // the run this session last touched

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

	// Every turn starts disarmed. The crash switch is per-request — arming it
	// once and leaving it set meant the resume of a crashed run crashed itself
	// on its first replayed tool, which is a demo that proves the opposite of
	// what it is for.
	s.crashAfter, s.crash, s.completed = 0, nil, 0
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

// Confirm implements tools.Approver.
func (s *Session) Confirm(action string) bool {
	approved, _ := s.Decide(action)
	return approved
}

// Decide shows the pending action in the page and waits for the human to click
// Approve or Deny — the same contract the terminal's y/n prompt has, with the
// answer arriving on a separate request instead of on stdin.
//
// The second return value is what makes the wait cheap. Before, every way of
// not getting a yes collapsed into "no": a closed tab, a five-minute timeout,
// and a deliberate refusal were indistinguishable, so walking away from your
// desk silently denied the action. Now only a click is an answer. Everything
// else reports "nobody answered", and the durable gate above parks the workflow
// on disk instead — where the same question can be answered tomorrow with
// `-approve <id>` and the run picks up exactly where it stopped.
//
// The timeout stays, but it now means "stop holding this request open", not
// "the human said no".
func (s *Session) Decide(action string) (approved, answered bool) {
	// Register before asking, so a decision that comes back immediately still
	// finds somewhere to land. The channel is buffered, so resolve never blocks
	// on a Decide that has already given up.
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
		return false, false // nothing is listening; there is no human to ask
	}

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case approved := <-reply:
		return approved, true
	case <-ctx.Done():
		return false, false // the browser went away mid-question
	case <-time.After(approvalTimeout):
		return false, false
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
		Type:   "tool",
		Name:   name,
		Args:   args,
		Result: result,
	})
}

func (s *Session) LogCompact(summary string) {
	_ = s.emit(event{Type: "compact", Text: summary})
}

// Emit implements events.Emitter: every harness event this session's agent
// produces is forwarded to the browser watching it, and nothing is filtered.
// The page's inspector pane is then the same stream the terminal renders, which
// is the point of having made everything an event in the first place.
//
// It is also where the two demo affordances live, both of which are the web
// layer's business rather than the harness's:
//
//   - the workflow id is remembered, so "Resume" is a button rather than an id
//     copied out of a log;
//   - the crash switch pulls the context once enough tools have completed.
//     Nothing in the harness knows it exists, because from a workflow's side a
//     cancelled context is exactly what a dying process looks like.
func (s *Session) Emit(e events.Event) {
	if e.Workflow != "" {
		s.wmu.Lock()
		s.workflow = e.Workflow
		s.wmu.Unlock()
	}

	_ = s.emit(event{Type: "harness", Harness: &e})

	if e.Type == events.ToolCompleted && s.crashAfter > 0 {
		s.completed++
		if s.completed >= s.crashAfter && s.crash != nil {
			s.emitStatus("crashed")
			s.crash()
		}
	}
}

// Workflow reports the run this session last touched, for the resume route.
func (s *Session) Workflow() string {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.workflow
}

func (s *Session) emitStatus(state string) { _ = s.emit(event{Type: "status", Text: state}) }

// arm sets up the crash switch for the turn that is starting — begin has
// already cleared it, so this only ever turns it on.
func (s *Session) arm(after int, cancel func()) {
	if after <= 0 {
		return
	}
	s.crashAfter, s.crash, s.completed = after, cancel, 0
}

// report turns whatever a turn returned into the last event the page sees.
//
// A parked workflow is not an error the user should be shown as one: nothing
// broke, a decision is outstanding, and the run is safe on disk until someone
// makes it. A cancelled context is not one either — that is the crash switch
// doing exactly what it was asked to.
func (s *Session) report(answer string, err error) {
	switch {
	case err == nil:
		s.emit(event{Type: "answer", Text: answer})
	case errors.Is(err, context.Canceled):
		s.emit(event{Type: "error", Text: "the run stopped mid-workflow — Resume picks it up where it died"})
	default:
		if parked, ok := durable.IsSuspended(err); ok {
			s.emit(event{Type: "suspended", Text: parked.Reason, ID: parked.ID})
			return
		}
		s.emit(event{Type: "error", Text: err.Error()})
	}
}
