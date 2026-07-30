package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// approve is a stub human: it says yes or no to every approval request.
type approve bool

func (a approve) Confirm(string) bool { return bool(a) }

// refuseToBeAsked fails the test if any tool requests approval — used to prove
// read-only tools never reach the human-in-the-loop gate.
type refuseToBeAsked struct{ t *testing.T }

func (r refuseToBeAsked) Confirm(action string) bool {
	r.t.Fatalf("a read-only tool asked for approval: %s", action)
	return false
}

// jsonArgs builds a tool's JSON argument string the way the model would.
func jsonArgs(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

// Requirement 3: write a script to disk, run it, read back the result.
func TestWriteRunReadRoundtrip(t *testing.T) {
	reg := Default(approve(true))
	ctx := context.Background()
	dir := t.TempDir()
	script := filepath.Join(dir, "hello.sh")
	result := filepath.Join(dir, "result.txt")

	if got := reg.Dispatch(ctx, "write_file", jsonArgs(t, map[string]any{
		"path":    script,
		"content": "#!/bin/sh\necho 'agent works' > " + result + "\n",
	})); !strings.Contains(got, "Wrote") {
		t.Fatalf("write_file: %q", got)
	}
	if got := reg.Dispatch(ctx, "run_command", jsonArgs(t, map[string]any{
		"command": "sh " + script,
	})); strings.Contains(got, "exit error") {
		t.Fatalf("run_command failed: %q", got)
	}
	if got := reg.Dispatch(ctx, "read_file", jsonArgs(t, map[string]any{
		"path": result,
	})); !strings.Contains(got, "agent works") {
		t.Fatalf("read_file got %q, want it to contain 'agent works'", got)
	}
}

// Requirement 4: edit an existing file.
func TestEditFile(t *testing.T) {
	reg := Default(approve(true))
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	if got := reg.Dispatch(context.Background(), "edit_file", jsonArgs(t, map[string]any{
		"path": path, "old_string": "world", "new_string": "gophers",
	})); !strings.Contains(got, "Edited") {
		t.Fatalf("edit_file: %q", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello gophers" {
		t.Fatalf("file is %q, want %q", string(b), "hello gophers")
	}
}

// Requirement 5: a "no" at the human-in-the-loop prompt blocks the action.
func TestHumanInLoopDenies(t *testing.T) {
	reg := Default(approve(false)) // human says NO to everything
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "should-not-exist.txt")

	if got := reg.Dispatch(ctx, "write_file", jsonArgs(t, map[string]any{
		"path": path, "content": "nope",
	})); !strings.Contains(got, "Denied") {
		t.Fatalf("expected denial, got %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file was written despite denial")
	}
	if got := reg.Dispatch(ctx, "run_command", jsonArgs(t, map[string]any{
		"command": "touch " + filepath.Join(dir, "x"),
	})); !strings.Contains(got, "Denied") {
		t.Fatalf("run_command should have been denied, got %q", got)
	}
}

// Read-only tools must never trigger the human-in-the-loop prompt.
func TestReadOnlyToolsNeedNoApproval(t *testing.T) {
	reg := Default(refuseToBeAsked{t})
	dir := t.TempDir()
	path := filepath.Join(dir, "r.txt")
	os.WriteFile(path, []byte("readable"), 0o644)

	if got := reg.Dispatch(context.Background(), "read_file", jsonArgs(t, map[string]any{
		"path": path,
	})); got != "readable" {
		t.Fatalf("read_file got %q", got)
	}
}

func TestUnknownToolIsHandled(t *testing.T) {
	reg := Default(approve(true))
	if got := reg.Dispatch(context.Background(), "no_such_tool", "{}"); !strings.Contains(got, "unknown tool") {
		t.Fatalf("got %q", got)
	}
}

// Requirement 2: web_search returns something usable. Network test — skipped
// with `go test -short`.
func TestWebSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("network test; skipped in -short mode")
	}
	tool := WebSearch{HTTP: http.DefaultClient, TavilyKey: os.Getenv("TAVILY_API_KEY")}
	got, err := tool.Run(context.Background(), jsonArgs(t, map[string]any{"query": "capital of France"}))
	if err != nil {
		t.Fatalf("web_search error: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("web_search returned empty")
	}
}
