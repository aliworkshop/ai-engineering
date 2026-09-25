package durable

import (
	"errors"
	"testing"
)

// A completed step is never run twice: the second pass returns the cached
// value without calling fn. That is the whole claim of the package.
func TestStepRunsOnceEver(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	sends := 0
	send := func() (string, error) { sends++; return "sent", nil }

	wf, err := store.Open("wf1", "email the customer")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := Step(wf, "tool-1", send); err != nil || out != "sent" {
		t.Fatalf("first pass: %q %v", out, err)
	}

	// The process "dies" and the workflow is reopened from disk.
	replay, err := store.Open("wf1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Resumed() {
		t.Fatal("a workflow loaded from disk should report itself resumed")
	}
	if replay.Input() != "email the customer" {
		t.Fatalf("input did not survive: %q", replay.Input())
	}
	if out, err := Step(replay, "tool-1", send); err != nil || out != "sent" {
		t.Fatalf("replay: %q %v", out, err)
	}
	if sends != 1 {
		t.Fatalf("the customer was emailed %d times", sends)
	}
}

// A failed step is NOT checkpointed — pinning a failure would make a crash
// permanent, and retrying the workflow has to mean retrying what broke.
func TestFailedStepIsNotCheckpointed(t *testing.T) {
	store, _ := NewStore(t.TempDir(), nil)
	wf, _ := store.Open("wf2", "task")

	if _, err := Step(wf, "flaky", func() (string, error) {
		return "", errors.New("boom")
	}); err == nil {
		t.Fatal("expected the error to surface")
	}
	if wf.Cached("flaky") {
		t.Fatal("a failed step was checkpointed; the crash is now permanent")
	}

	if out, err := Step(wf, "flaky", func() (string, error) { return "ok", nil }); err != nil || out != "ok" {
		t.Fatalf("retry: %q %v", out, err)
	}
}

// Pending is what recovery looks for: workflows still marked running with
// nobody running them.
func TestPendingFindsOnlyUnfinishedWork(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir, nil)

	done, _ := store.Open("finished", "a")
	if err := done.Finish(StatusDone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("crashed", "b"); err != nil {
		t.Fatal(err)
	}

	pending, err := store.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "crashed" {
		t.Fatalf("pending = %+v, want just the crashed one", pending)
	}
}
