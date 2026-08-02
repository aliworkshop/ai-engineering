// Package web is the browser front-end. It serves one chat page, gives every
// browser session its own conversation, and keeps the same human-in-the-loop
// gate the terminal has — the y/n that ui.Console asks on stdin is asked here
// as an Approve/Deny card in the page, and the tool stays blocked until the
// human clicks.
//
// The layering matches the terminal UI: this package knows the agent, the agent
// knows an abstract tool box, the tools know an abstract approver. A *Session
// plays the part ui.Console plays there — it is both the approver and the thing
// progress is reported to.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Assistant is all the server needs from the agent: ask a question, get an
// answer. *agent.Agent satisfies it. Depending on the behaviour rather than the
// concrete type keeps this package testable without a model behind it.
type Assistant interface {
	Ask(ctx context.Context, input string) (string, error)
}

// NewAssistant builds the assistant for one browser session. It is handed the
// Session, which stands in for the human at the far end of the browser: the
// Session satisfies tools.Approver, and its LogTool / LogCompact methods are
// what the agent's progress hooks should be wired to.
type NewAssistant func(*Session) Assistant

const (
	// sessionCookie ties a browser to its conversation. Without it every request
	// would start from an empty history and follow-up questions would lose their
	// context.
	sessionCookie = "agent_session"

	// sessionTTL is how long an idle conversation is kept before it's dropped.
	// Sessions hold a full history in memory, so they can't accumulate forever.
	sessionTTL = 2 * time.Hour

	// maxMessageBytes caps one submitted question. The agent pays for every byte
	// it forwards to the model, so a runaway paste is refused rather than billed.
	maxMessageBytes = 64 << 10

	// canvasFile is the diagram the tools draw. The page shows it next to the
	// chat so a "draw me a flowchart" answer is visible without leaving the tab.
	canvasFile = "canvas.svg"
)

// Server owns the sessions and the routes. The zero value is not usable; call
// New.
type Server struct {
	newAssistant NewAssistant

	// Dir is where the diagram tools write canvas.svg. Empty means the working
	// directory, which is where the tools default to as well.
	Dir string

	// EditorDir is the excalidraw/ folder the diagram editor is served out of.
	// Empty means there isn't one, and the page falls back to showing the
	// drawing without offering to edit it. New fills it in.
	EditorDir string

	// mu guards sessions and every session's lastSeen field. Turns themselves
	// are serialized per session by Session.turn, not here, so a long turn in
	// one browser never blocks a request from another.
	mu       sync.Mutex
	sessions map[string]*Session
}

// New returns a server that builds each session's conversation with the given
// factory.
func New(newAssistant NewAssistant) *Server {
	return &Server{
		newAssistant: newAssistant,
		sessions:     make(map[string]*Session),
		EditorDir:    defaultEditorDir(),
	}
}

// Handler returns the routes: the page itself, the chat stream, the approval
// reply, a reset, the diagram the tools draw, and the editor that draws over it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("POST /api/approve", s.handleApprove)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	mux.HandleFunc("GET /"+canvasFile, s.handleCanvas)
	mux.HandleFunc("GET /"+sceneFile, s.handleScene)
	mux.HandleFunc("POST /api/canvas", s.handleSaveCanvas)
	mux.Handle("GET /editor/", s.editorHandler())
	return mux
}

// ListenAndServe runs the browser UI on addr until it fails.
//
// There is deliberately no WriteTimeout: a turn is a single streaming response
// that stays open for as long as the agent works, and a write deadline would
// cut it off mid-thought. ReadHeaderTimeout still guards the cheap half.
func ListenAndServe(addr string, newAssistant NewAssistant) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           New(newAssistant).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Hand out the cookie here so the page already has a session before its
	// first question — otherwise the cookie would have to be set on the chat
	// response, which is a stream we've already started writing.
	s.session(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// handleChat runs one turn and streams what happens as server-sent events: a
// line per tool call, a card per approval request, then the final answer. The
// response stays open for the whole turn, which is why the events go out as a
// stream rather than one JSON reply at the end — a turn can take several model
// round trips, and a dead page for all of them is exactly what the terminal
// spinner exists to avoid.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes)).Decode(&req); err != nil {
		http.Error(w, "could not read the message", http.StatusBadRequest)
		return
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		http.Error(w, "the message is empty", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "this server cannot stream", http.StatusInternalServerError)
		return
	}

	sess := s.session(w, r)

	// One conversation, one turn at a time. The agent mutates its history as it
	// works, so a second question arriving mid-turn would interleave with it.
	// Refusing is better than queueing: the page can tell the user why.
	if !sess.turn.TryLock() {
		http.Error(w, "this conversation is already answering a question", http.StatusConflict)
		return
	}
	defer sess.turn.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer a response would hold every event until the turn ends,
	// which is the one thing this stream exists to prevent.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sess.begin(r.Context(), &sseWriter{w: w, flush: flusher.Flush})
	defer sess.end()

	answer, err := sess.assistant.Ask(r.Context(), message)
	if err != nil {
		sess.emit(event{Type: "error", Text: err.Error()})
		return
	}
	sess.emit(event{Type: "answer", Text: answer})
}

// handleApprove delivers the human's y/n back to the tool that is blocked
// waiting for it. It's a separate request because the turn's own response is
// busy being a stream at that moment.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "could not read the decision", http.StatusBadRequest)
		return
	}
	if !s.session(w, r).resolve(req.ID, req.Approved) {
		// Either the turn ended, the request timed out, or this is a stale click
		// on a card the page still shows.
		http.Error(w, "that approval is no longer waiting", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleReset starts the conversation over with a fresh history — the browser's
// equivalent of quitting the CLI and running it again.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	if !sess.turn.TryLock() {
		http.Error(w, "this conversation is already answering a question", http.StatusConflict)
		return
	}
	defer sess.turn.Unlock()

	sess.assistant = s.newAssistant(sess)
	w.WriteHeader(http.StatusNoContent)
}

// handleCanvas serves the diagram the tools draw, so the page can show it
// instead of asking the user to open a file. The page displays it in an <img>,
// where an SVG can't run script, so model-supplied labels reaching this file
// stay inert. Nothing is cached: every redraw overwrites the same path.
func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	svg, err := os.ReadFile(filepath.Join(s.Dir, canvasFile))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "script-src 'none'")
	_, _ = w.Write(svg)
}

// session returns the conversation this browser owns, starting one — and
// handing out the cookie that names it — the first time we see the browser.
// Callers must not have written the response body yet: a new session sets a
// header.
func (s *Server) session(w http.ResponseWriter, r *http.Request) *Session {
	id := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		id = c.Value
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropIdleLocked()

	if sess, ok := s.sessions[id]; ok {
		sess.lastSeen = time.Now()
		return sess
	}

	// Unknown or expired cookie: a fresh conversation under an id we generate
	// ourselves, never one the browser supplied.
	sess := newSession(s.newAssistant)
	s.sessions[sess.id] = sess
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return sess
}

// dropIdleLocked forgets conversations nobody has touched in sessionTTL. It
// runs on each lookup rather than on a timer — this is a single-user dev tool,
// and a sweep goroutine would outlive every request for no benefit.
func (s *Server) dropIdleLocked() {
	cutoff := time.Now().Add(-sessionTTL)
	for id, sess := range s.sessions {
		if sess.lastSeen.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
}

// newID returns an unguessable session id. Session ids are the only thing
// separating one browser's conversation from another's, so they come from
// crypto/rand.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we run on; if it somehow
		// did, falling back to a predictable id would be worse than stopping.
		panic("web: no randomness for session ids: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
