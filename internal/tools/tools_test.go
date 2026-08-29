package tools

import (
	"context"
	"encoding/json"
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

// Requirement 3: write a file to disk, read back what landed there.
func TestWriteReadRoundtrip(t *testing.T) {
	reg := Default(approve(true))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "result.txt")

	if got := reg.Dispatch(ctx, "write_file", jsonArgs(t, map[string]any{
		"path":    path,
		"content": "agent works\n",
	})); !strings.Contains(got, "Wrote") {
		t.Fatalf("write_file: %q", got)
	}
	if got := reg.Dispatch(ctx, "read_file", jsonArgs(t, map[string]any{
		"path": path,
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
	keep := filepath.Join(dir, "keep.txt")
	os.WriteFile(keep, []byte("still here"), 0o644)
	if got := reg.Dispatch(ctx, "delete_file", jsonArgs(t, map[string]any{
		"path": keep,
	})); !strings.Contains(got, "Denied") {
		t.Fatalf("delete_file should have been denied, got %q", got)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("file was deleted despite denial")
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
