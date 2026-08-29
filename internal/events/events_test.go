package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestJSONLIsAppendOnlyAndParsable is what makes the "cost and latency from the
// log" exercise possible: every event is one complete, self-describing line.
func TestJSONLIsAppendOnlyAndParsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "events.jsonl")
	log := NewJSONL(path)

	approved := true
	Emit(log, Event{Type: WorkflowStarted, Workflow: "abc", Text: "delete the file"})
	Emit(log, Event{Type: ToolCompleted, Workflow: "abc", Name: "get_weather", Millis: 12})
	Emit(log, Event{Type: ApprovalResolved, Workflow: "abc", Approved: &approved})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), raw)
	}

	var last Event
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &last); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i+1, err, line)
		}
		if last.TS == "" {
			t.Fatalf("line %d has no timestamp; latency reporting needs one", i+1)
		}
	}
	if last.Approved == nil || !*last.Approved {
		t.Fatalf("a human's yes did not survive the round trip: %+v", last)
	}

	// Empty fields stay out of the log — a wall of empty keys is unreadable.
	if strings.Contains(lines[0], `"name"`) {
		t.Fatalf("empty fields should be omitted: %s", lines[0])
	}
}

// TestApprovedDistinguishesNoFromUnasked is why Approved is a pointer: a
// refusal and an unanswered question are different facts.
func TestApprovedDistinguishesNoFromUnasked(t *testing.T) {
	refused := false
	denied := Event{Type: ApprovalResolved, Approved: &refused}
	pending := Event{Type: ApprovalRequested}

	if !strings.Contains(denied.JSON(), `"approved":false`) {
		t.Fatalf("an explicit refusal must be recorded as false: %s", denied.JSON())
	}
	if strings.Contains(pending.JSON(), "approved") {
		t.Fatalf("an unanswered request must not claim a verdict: %s", pending.JSON())
	}
}

// panicSink is a broken consumer. Observability must never be the reason a
// workflow dies.
type panicSink struct{}

func (panicSink) Emit(Event) { panic(errors.New("this sink is broken")) }

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) Emit(Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func TestABrokenSinkCannotTakeDownTheOthers(t *testing.T) {
	good := &counter{}
	bus := Bus{panicSink{}, good, nil}

	Emit(bus, Event{Type: WorkflowStarted})
	Emit(bus, Event{Type: WorkflowCompleted})

	if good.n != 2 {
		t.Fatalf("the working sink received %d of 2 events", good.n)
	}
}

// TestNilEmitterIsLegal keeps the harness usable from a unit test that has no
// interest in events.
func TestNilEmitterIsLegal(t *testing.T) {
	got := Emit(nil, Event{Type: ToolRequested, Name: "get_weather"})
	if got.TS == "" {
		t.Fatalf("the event should still be stamped and returned")
	}
}

// TestQuietConsoleKeepsStateChanges: the terminal UI prints its own tool lines,
// so the quiet renderer drops per-step noise but must keep the events that say
// something changed.
func TestQuietConsoleKeepsStateChanges(t *testing.T) {
	var buf bytes.Buffer
	console := NewConsole(&buf)
	console.Quiet = true

	Emit(console, Event{Type: ToolRequested, Name: "get_weather"})
	Emit(console, Event{Type: ModelCompleted})
	Emit(console, Event{Type: ApprovalRequested, Workflow: "abc", Text: "DELETE prod.db"})
	Emit(console, Event{Type: AgentHandoff, From: "assistant", To: "operator"})

	out := buf.String()
	if strings.Contains(out, "get_weather") || strings.Contains(out, "model.completed") {
		t.Fatalf("quiet mode should drop routine per-step chatter:\n%s", out)
	}
	for _, want := range []string{"approval.requested", "DELETE prod.db", "agent.handoff", "operator"} {
		if !strings.Contains(out, want) {
			t.Fatalf("quiet mode dropped %q, which is a state change:\n%s", want, out)
		}
	}
}

// TestConsoleKeepsOneLinePerEvent: a multi-line tool result must not break the
// one-glyph-one-line reading the stream depends on.
func TestConsoleKeepsOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	console := NewConsole(&buf)

	Emit(console, Event{Type: ToolCompleted, Name: "get_weather", Result: "line one\nline two\nline three"})

	if n := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("expected exactly one line, got %d extra:\n%s", n, buf.String())
	}
}
