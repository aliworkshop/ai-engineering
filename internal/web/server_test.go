package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// scripted stands in for the agent: whatever the test wants a turn to do, it
// does. Because the server depends on the Assistant interface, none of these
// tests need a model, an API key, or a network.
type scripted struct {
	session *Session
	ask     func(*Session, string) (string, error)
}

func (a scripted) Ask(_ context.Context, input string) (string, error) {
	return a.ask(a.session, input)
}

// newTestServer starts the real handler over a scripted assistant, with a
// cookie-keeping client so successive requests land on the same conversation —
// the way a browser behaves.
func newTestServer(t *testing.T, ask func(*Session, string) (string, error)) (*Server, *httptest.Server, *http.Client) {
	t.Helper()

	srv := New(func(s *Session) Assistant { return scripted{session: s, ask: ask} })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts, newBrowser(t, ts)
}

// newBrowser returns a client that has already loaded the page, so it holds a
// session cookie before it asks anything — the order a real browser does it in.
// Tests that fire concurrent requests depend on this: two cookie-less requests
// racing would each be handed a session of their own.
func newBrowser(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("load the page: %v", err)
	}
	defer res.Body.Close()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("load the page: %v", err)
	}
	return client
}

// post sends JSON and returns the raw response; the caller closes the body.
func post(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return res
}

// readEvents parses the turn's stream, handing each event to on as it arrives —
// which is what lets a test answer an approval while the turn is still blocked
// on it — and returns them all once the turn ends.
func readEvents(t *testing.T, body io.Reader, on func(event)) []event {
	t.Helper()

	var events []event
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue // the blank line that ends a frame
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		events = append(events, e)
		if on != nil {
			on(e)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return events
}

// ask runs one turn and returns everything it streamed.
func ask(t *testing.T, ts *httptest.Server, client *http.Client, message string, on func(event)) []event {
	t.Helper()

	res := post(t, client, ts.URL+"/api/chat", map[string]string{"message": message})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("chat: got %d: %s", res.StatusCode, body)
	}
	return readEvents(t, res.Body, on)
}

func find(events []event, kind string) (event, bool) {
	for _, e := range events {
		if e.Type == kind {
			return e, true
		}
	}
	return event{}, false
}

// A turn reports what it did as it goes and ends with the answer, so a page
// watching the stream shows its work instead of sitting silent for the whole
// turn.
func TestTurnStreamsToolCallsThenTheAnswer(t *testing.T) {
	_, ts, client := newTestServer(t, func(s *Session, input string) (string, error) {
		s.LogTool("get_weather", `{"location":"Tokyo"}`, "18.2°C")
		s.LogCompact("earlier turns, folded")
		return "you said: " + input, nil
	})

	events := ask(t, ts, client, "hi", nil)

	tool, ok := find(events, "tool")
	if !ok {
		t.Fatalf("no tool event in %+v", events)
	}
	if tool.Name != "get_weather" || tool.Result != "18.2°C" {
		t.Fatalf("tool event lost detail: %+v", tool)
	}
	if _, ok := find(events, "compact"); !ok {
		t.Fatalf("no compaction event in %+v", events)
	}
	answer, ok := find(events, "answer")
	if !ok {
		t.Fatalf("no answer event in %+v", events)
	}
	if answer.Text != "you said: hi" {
		t.Fatalf("wrong answer: %q", answer.Text)
	}
	if last := events[len(events)-1]; last.Type != "answer" {
		t.Fatalf("the answer should end the turn, got %q last", last.Type)
	}
}

// A failed turn has to say so on the stream: the response is already a 200 by
// the time the model fails, so an HTTP status can't carry the error.
func TestFailedTurnStreamsTheError(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) {
		return "", fmt.Errorf("model exploded")
	})

	events := ask(t, ts, client, "hi", nil)

	failure, ok := find(events, "error")
	if !ok {
		t.Fatalf("no error event in %+v", events)
	}
	if !strings.Contains(failure.Text, "model exploded") {
		t.Fatalf("error event lost the reason: %q", failure.Text)
	}
}

// The whole point of the gate: the tool stays blocked until the human clicks,
// and gets back exactly what they clicked.
func TestApprovalBlocksTheToolUntilTheHumanAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approved bool
	}{
		{"approved", true},
		{"denied", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ts, client := newTestServer(t, func(s *Session, _ string) (string, error) {
				return fmt.Sprintf("verdict=%v", s.Confirm("rm -rf /tmp/x")), nil
			})

			// Answering from inside the reader is the point: the turn is still
			// open, parked inside Confirm, when the decision goes out.
			events := ask(t, ts, client, "delete it", func(e event) {
				if e.Type != "approval" {
					return
				}
				res := post(t, client, ts.URL+"/api/approve", map[string]any{
					"id": e.ID, "approved": tc.approved,
				})
				defer res.Body.Close()
				if res.StatusCode != http.StatusNoContent {
					t.Errorf("approve: got %d, want 204", res.StatusCode)
				}
			})

			request, ok := find(events, "approval")
			if !ok {
				t.Fatalf("the tool never asked for approval: %+v", events)
			}
			if request.Action != "rm -rf /tmp/x" {
				t.Fatalf("approval lost the action: %q", request.Action)
			}
			answer, _ := find(events, "answer")
			if want := fmt.Sprintf("verdict=%v", tc.approved); answer.Text != want {
				t.Fatalf("tool saw %q, want %q", answer.Text, want)
			}
		})
	}
}

// A click that arrives after its turn ended must not be mistaken for a fresh
// answer — nor crash the server looking for a channel that's gone.
func TestStaleApprovalIsRejected(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) {
		return "done", nil
	})
	ask(t, ts, client, "hi", nil)

	res := post(t, client, ts.URL+"/api/approve", map[string]any{"id": "nope", "approved": true})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("stale approval: got %d, want 404", res.StatusCode)
	}
}

// With no turn running there is no human watching, so a tool asking for
// approval is refused rather than left hanging.
func TestApprovalOutsideATurnIsDenied(t *testing.T) {
	session := newSession(func(s *Session) Assistant {
		return scripted{session: s, ask: func(*Session, string) (string, error) { return "", nil }}
	})
	if session.Confirm("rm -rf /") {
		t.Fatal("approved an action nobody could see")
	}
}

// One conversation answers one question at a time — the agent rewrites its
// history as it works, so an overlapping turn would corrupt it.
func TestSecondQuestionDuringATurnIsRefused(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	// Releasing on cleanup too, so a failed assertion doesn't leave the handler
	// parked here — httptest.Server.Close waits for its requests to finish.
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)

	_, ts, client := newTestServer(t, func(*Session, string) (string, error) {
		close(started)
		<-release
		return "done", nil
	})

	first := make(chan []event, 1)
	go func() {
		// Deferred so a failure inside ask — which ends this goroutine on the
		// spot — still reports back instead of hanging the test.
		var events []event
		defer func() { first <- events }()
		events = ask(t, ts, client, "slow one", nil)
	}()
	<-started // the turn is genuinely in flight now

	res := post(t, client, ts.URL+"/api/chat", map[string]string{"message": "me too"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second question: got %d, want 409", res.StatusCode)
	}

	unblock()
	if answer, ok := find(<-first, "answer"); !ok || answer.Text != "done" {
		t.Fatalf("the first turn should have finished normally, got %+v", answer)
	}
}

// Two browsers must not share a history: the cookie is what separates them.
func TestEachBrowserGetsItsOwnConversation(t *testing.T) {
	seen := make(chan *Session, 2)
	srv, ts, client := newTestServer(t, func(s *Session, _ string) (string, error) {
		seen <- s
		return "ok", nil
	})

	ask(t, ts, client, "first browser", nil)
	ask(t, ts, newBrowser(t, ts), "second browser", nil)

	if a, b := <-seen, <-seen; a == b {
		t.Fatal("both browsers landed on the same conversation")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(srv.sessions))
	}
}

// A follow-up question has to reach the same conversation, or the agent's
// history — the whole reason it can answer follow-ups — is useless.
func TestFollowUpReachesTheSameConversation(t *testing.T) {
	seen := make(chan *Session, 2)
	_, ts, client := newTestServer(t, func(s *Session, _ string) (string, error) {
		seen <- s
		return "ok", nil
	})

	ask(t, ts, client, "first", nil)
	ask(t, ts, client, "follow-up", nil)

	if a, b := <-seen, <-seen; a != b {
		t.Fatal("the follow-up started a new conversation")
	}
}

// Reset is the browser's "quit and start again": same session, empty history.
func TestResetSwapsInAFreshAssistant(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "ok", nil })

	ask(t, ts, client, "hi", nil)
	res := post(t, client, ts.URL+"/api/reset", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: got %d, want 204", res.StatusCode)
	}
}

// An empty question would cost a model round trip to answer with nothing.
func TestEmptyQuestionIsRefused(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) {
		t.Error("the agent should never have been asked")
		return "", nil
	})

	res := post(t, client, ts.URL+"/api/chat", map[string]string{"message": "   "})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty question: got %d, want 400", res.StatusCode)
	}
}

// The page is the app: it has to come back from the binary, with a session
// cookie already set so the first question isn't a stranger.
func TestIndexServesThePageAndStartsASession(t *testing.T) {
	_, ts, _ := newTestServer(t, func(*Session, string) (string, error) { return "", nil })

	// A browser that has never been here before: no cookie on the way in, so
	// one has to come back with the page.
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("<title>English teacher</title>")) {
		t.Fatalf("got %d and %d bytes, want the chat page", res.StatusCode, len(body))
	}
	if len(res.Cookies()) == 0 {
		t.Fatal("no session cookie handed out with the page")
	}
}

// Idle conversations hold a whole history in memory, so they have to age out.
func TestIdleConversationsAreDropped(t *testing.T) {
	srv, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "ok", nil })

	ask(t, ts, client, "hi", nil)

	srv.mu.Lock()
	for _, sess := range srv.sessions {
		sess.lastSeen = time.Now().Add(-2 * sessionTTL)
	}
	srv.mu.Unlock()

	// Any lookup sweeps; a second browser is the cheapest way to trigger one.
	ask(t, ts, newBrowser(t, ts), "hi", nil)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.sessions) != 1 {
		t.Fatalf("got %d sessions, want only the live one", len(srv.sessions))
	}
}

// --- the inspector, the runtime controls, and the crash switch --------------

// resumable is a scripted assistant that can also be resumed, so the resume
// route has something to drive without a model behind it.
type resumable struct {
	scripted
	resume func(*Session, string) (string, error)
}

func (a resumable) Resume(_ context.Context, id string) (string, error) {
	return a.resume(a.session, id)
}

// Everything the harness emits reaches the browser, not just the tool calls.
// The inspector pane is the reason: a page that shows a redaction of the event
// stream is a worse debugger than the terminal, which shows all of it.
func TestHarnessEventsReachTheBrowser(t *testing.T) {
	_, ts, client := newTestServer(t, func(s *Session, _ string) (string, error) {
		s.Emit(events.Event{Type: events.WorkflowStarted, Workflow: "wf1", Text: "a task"})
		s.Emit(events.Event{Type: events.ToolRequested, Workflow: "wf1", Name: "search_knowledge", Call: "call_1"})
		s.Emit(events.Event{Type: events.MemoryCompacted, Workflow: "wf1", Text: "folded 3 turns"})
		return "done", nil
	})

	stream := ask(t, ts, client, "hi", nil)

	var seen []string
	for _, e := range stream {
		if e.Type == "harness" {
			if e.Harness == nil {
				t.Fatal("a harness event arrived with nothing in it")
			}
			seen = append(seen, string(e.Harness.Type))
		}
	}
	want := []string{"workflow.started", "tool.requested", "memory.compacted"}
	if len(seen) != len(want) {
		t.Fatalf("forwarded %v, want %v", seen, want)
	}
	for i, kind := range want {
		if seen[i] != kind {
			t.Fatalf("event %d is %q, want %q — order is what makes the pane readable", i, seen[i], kind)
		}
	}
}

// The crash switch pulls the context once enough tools have completed, which is
// what a dying process looks like from the workflow's side. The turn ends
// without an answer, and the run is left on disk for Resume.
func TestCrashSwitchStopsTheTurn(t *testing.T) {
	var reached bool
	_, ts, client := newTestServer(t, func(s *Session, _ string) (string, error) {
		s.Emit(events.Event{Type: events.ToolCompleted, Workflow: "wf1", Name: "search_knowledge", Call: "c1"})
		if err := s.ctx.Err(); err != nil {
			return "", err // the harness would unwind here too
		}
		reached = true
		return "should not get here", nil
	})

	res := post(t, client, ts.URL+"/api/chat", map[string]any{"message": "hi", "crash_after": 1})
	defer res.Body.Close()
	stream := readEvents(t, res.Body, nil)

	if reached {
		t.Fatal("the turn carried on after the crash switch fired")
	}
	status, ok := find(stream, "status")
	if !ok || status.Text != "crashed" {
		t.Fatalf("no crashed status on the stream: %+v", stream)
	}
	if _, ok := find(stream, "answer"); ok {
		t.Fatal("a crashed turn must not produce an answer")
	}
}

// And the switch is per request. Leaving it armed meant the resume of a crashed
// run crashed itself on its first replayed tool — a demo that proves the
// opposite of what it is for.
func TestCrashSwitchDoesNotLeakIntoTheNextTurn(t *testing.T) {
	_, ts, client := newTestServer(t, func(s *Session, input string) (string, error) {
		s.Emit(events.Event{Type: events.ToolCompleted, Workflow: "wf1", Name: "t", Call: "c1"})
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		return "answered: " + input, nil
	})

	crashed := post(t, client, ts.URL+"/api/chat", map[string]any{"message": "one", "crash_after": 1})
	readEvents(t, crashed.Body, nil)
	crashed.Body.Close()

	stream := ask(t, ts, client, "two", nil)
	answer, ok := find(stream, "answer")
	if !ok || answer.Text != "answered: two" {
		t.Fatalf("the second turn was still armed: %+v", stream)
	}
}

// Resume is the browser half of Part 2 and Part 7: until it existed, the
// runtime could park a run that only a terminal could revive.
func TestResumePicksTheRunBackUp(t *testing.T) {
	var resumed string
	srv := New(func(s *Session) Assistant {
		return resumable{
			scripted: scripted{session: s, ask: func(*Session, string) (string, error) { return "", nil }},
			resume: func(s *Session, id string) (string, error) {
				resumed = id
				s.Emit(events.Event{Type: events.StepCached, Workflow: id, Name: "tool-c1"})
				return "finished after resuming", nil
			},
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := newBrowser(t, ts)

	res := post(t, client, ts.URL+"/api/resume", map[string]string{"id": "wf-42"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("resume: got %d: %s", res.StatusCode, body)
	}
	stream := readEvents(t, res.Body, nil)

	if resumed != "wf-42" {
		t.Fatalf("resumed %q, want wf-42", resumed)
	}
	if status, ok := find(stream, "status"); !ok || status.Text != "recovering" {
		t.Fatalf("the page was never told it was recovering: %+v", stream)
	}
	if answer, ok := find(stream, "answer"); !ok || answer.Text != "finished after resuming" {
		t.Fatalf("no answer from the resumed run: %+v", stream)
	}
}

// An assistant that cannot resume says so, rather than the page pretending the
// button did something.
func TestResumeIsHonestWhenUnsupported(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "", nil })

	res := post(t, client, ts.URL+"/api/resume", map[string]string{"id": "wf-1"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501", res.StatusCode)
	}
}

// The controls that reach the runtime's own files are optional, and absent ones
// must degrade rather than 500: the page asks for them on load.
func TestRuntimeControlsAreOptional(t *testing.T) {
	_, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "", nil })

	res, err := client.Get(ts.URL + "/api/workflows")
	if err != nil {
		t.Fatalf("GET workflows: %v", err)
	}
	defer res.Body.Close()
	var rows []Workflow
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.StatusCode != http.StatusOK || len(rows) != 0 {
		t.Fatalf("got %d and %v, want 200 and an empty list", res.StatusCode, rows)
	}

	clear := post(t, client, ts.URL+"/api/clear", nil)
	defer clear.Body.Close()
	if clear.StatusCode != http.StatusNotImplemented {
		t.Fatalf("clear without a hook: got %d, want 501", clear.StatusCode)
	}
}

// With the hooks wired, both controls do what they say.
func TestRuntimeControlsRunTheirHooks(t *testing.T) {
	cleared := false
	srv := New(func(s *Session) Assistant {
		return scripted{session: s, ask: func(*Session, string) (string, error) { return "", nil }}
	})
	WithWorkflows(func() ([]Workflow, error) {
		return []Workflow{{ID: "wf1", Status: "suspended", Waiting: "DELETE /tmp/x"}}, nil
	})(srv)
	WithReset(func() error { cleared = true; return nil })(srv)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := newBrowser(t, ts)

	res, err := client.Get(ts.URL + "/api/workflows")
	if err != nil {
		t.Fatalf("GET workflows: %v", err)
	}
	defer res.Body.Close()
	var rows []Workflow
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].Waiting == "" {
		t.Fatalf("the parked run and what it waits on must both survive: %+v", rows)
	}

	clear := post(t, client, ts.URL+"/api/clear", nil)
	defer clear.Body.Close()
	if clear.StatusCode != http.StatusNoContent || !cleared {
		t.Fatalf("clear: got %d, hook ran: %v", clear.StatusCode, cleared)
	}
}
