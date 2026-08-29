package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// indexHTML is the whole front-end: one page, no build step, no CDN. Embedding
// it means the binary is the app — `go run .` serves a working UI from any
// directory.
//
//go:embed index.html
var indexHTML []byte

// event is one thing that happened during a turn. The page switches on Type:
//
//	tool      a tool ran           — Name, Args, Result
//	compact   history was folded   — Text (the summary)
//	approval  a tool needs a y/n   — ID, Action
//	answer    the turn is done     — Text
//	suspended nobody answered; the run is parked on disk — ID, Text
//	error     the turn failed      — Text
//	status    the run changed state — Text (idle, working, parked, recovering…)
//	harness   one typed harness event, verbatim — Harness
//
// That last one is the whole event stream forwarded as-is, and it is what the
// inspector pane renders. The terminal front-end has always shown every event;
// a browser that only sees tool calls is looking at a redaction of the same
// run.
type event struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action,omitempty"`
	Text   string `json:"text,omitempty"`

	Harness *events.Event `json:"harness,omitempty"`
}

// sseWriter writes server-sent events to a response that stays open for the
// whole turn. Flushing after every event is the point: without it the events
// would sit in the buffer until the turn ended, which is the same silence a
// plain JSON reply would give.
type sseWriter struct {
	w     http.ResponseWriter
	flush func()
}

// send writes one event. Each is a single "data:" line — JSON escapes newlines,
// so no payload can ever break the one-line-per-event framing the page parses.
func (s *sseWriter) send(e event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.flush()
	return nil
}
