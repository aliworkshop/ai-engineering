package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

func scratch(t *testing.T) string {
	t.Helper()
	dir, cleanup, err := Scratch(filepath.Join(t.TempDir(), "sandbox"))
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	t.Cleanup(cleanup)
	return dir
}

// TestRunCapturesOutput is the happy path: code runs, stdout comes back.
func TestRunCapturesOutput(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	script, err := WriteScript(dir, "program.py", "print(6 * 7)")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	res := Run(context.Background(), Config{Dir: dir}, "python3", script)
	if !res.OK {
		t.Fatalf("expected success, got %+v", res)
	}
	if strings.TrimSpace(res.Output) != "42" {
		t.Fatalf("output = %q, want 42", res.Output)
	}
}

// TestSecretsAreNotInherited is the cheapest, highest-value guarantee in the
// package: code the model wrote cannot read the key the agent is using.
func TestSecretsAreNotInherited(t *testing.T) {
	requirePython(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-do-not-leak")
	dir := scratch(t)

	script, _ := WriteScript(dir, "program.py",
		"import os; print(os.environ.get('OPENROUTER_API_KEY', 'ABSENT'))")

	res := Run(context.Background(), Config{Dir: dir}, "python3", script)
	if !res.OK {
		t.Fatalf("run failed: %+v", res)
	}
	if strings.Contains(res.Output, "sk-do-not-leak") {
		t.Fatalf("the sandbox leaked a credential: %q", res.Output)
	}
	if strings.TrimSpace(res.Output) != "ABSENT" {
		t.Fatalf("expected the variable to be absent, got %q", res.Output)
	}
}

// TestRunawayCodeIsKilled covers the loop that never ends. The Result must come
// back as a timeout, not as a hung test.
func TestRunawayCodeIsKilled(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	script, _ := WriteScript(dir, "program.py", "while True:\n    pass\n")

	start := time.Now()
	res := Run(context.Background(), Config{Dir: dir, Timeout: 700 * time.Millisecond}, "python3", script)
	elapsed := time.Since(start)

	if res.OK || !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the timeout did not fire promptly: took %s", elapsed)
	}
	if !strings.Contains(res.Error, "timed out") {
		t.Fatalf("expected a clear timeout error, got %q", res.Error)
	}
}

// TestBackgroundedChildDiesWithTheGroup is why the process group exists:
// killing the interpreter alone would leave the sleep running.
func TestBackgroundedChildDiesWithTheGroup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	dir := scratch(t)
	marker := filepath.Join(dir, "survivor.txt")

	// Spawn a child that will write the marker AFTER the parent's timeout, then
	// hang the parent so the timeout fires while the child is still pending.
	script, _ := WriteScript(dir, "program.sh",
		"( sleep 2; echo alive > "+marker+" ) &\nsleep 30\n")

	res := Run(context.Background(), Config{Dir: dir, Timeout: 500 * time.Millisecond}, "bash", script)
	if !res.TimedOut {
		t.Fatalf("expected the parent to time out, got %+v", res)
	}

	time.Sleep(3 * time.Second) // long enough for an orphan to have written
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("a backgrounded child outlived the sandbox and wrote %s", marker)
	}
}

// TestOutputIsCapped keeps a chatty program from becoming a memory problem.
func TestOutputIsCapped(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	script, _ := WriteScript(dir, "program.py", "print('x' * 500000)")

	res := Run(context.Background(), Config{Dir: dir, MaxOutput: 2048}, "python3", script)
	if len(res.Output) > 4096 {
		t.Fatalf("output was not capped: %d bytes", len(res.Output))
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("truncation should be announced, not silent: %q", res.Output[max(0, len(res.Output)-80):])
	}
}

// TestCrashIsAResultNotAnError: a program that blows up is information for the
// model to reason about, not a harness failure.
func TestCrashIsAResultNotAnError(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	script, _ := WriteScript(dir, "program.py", "raise ValueError('nope')")

	res := Run(context.Background(), Config{Dir: dir}, "python3", script)
	if res.OK {
		t.Fatalf("expected the crash to be reported")
	}
	if !strings.Contains(res.Output, "ValueError") {
		t.Fatalf("the traceback should reach the model: %q", res.Output)
	}
}

// TestBridgeExposesOnlyAllowedTools is the security story of code mode: a tool
// that is not on the allowlist does not exist inside the sandbox.
func TestBridgeExposesOnlyAllowedTools(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	var called []string
	bridge, err := Serve(dir, func(_ context.Context, name, args string) string {
		called = append(called, name)
		return `{"ok":true,"echo":` + args + `}`
	}, []string{"get_weather"})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer bridge.Close()

	if err := WriteShims(dir); err != nil {
		t.Fatalf("shims: %v", err)
	}

	script, _ := WriteScript(dir, "program.py", `
import agent_tools
print("ALLOWED:", agent_tools.call("get_weather", location="Tokyo"))
try:
    agent_tools.call("change_thing", what="/etc/passwd")
    print("LEAKED")
except RuntimeError as e:
    print("BLOCKED:", e)
`)

	cfg := Config{Dir: dir, Extra: bridge.Env()}
	res := Run(context.Background(), cfg, "python3", script)
	if !res.OK {
		t.Fatalf("bridge run failed: %+v", res)
	}
	if !strings.Contains(res.Output, "ALLOWED:") {
		t.Fatalf("the allowed tool did not come through: %q", res.Output)
	}
	if strings.Contains(res.Output, "LEAKED") {
		t.Fatalf("sandboxed code reached a tool that was never exposed: %q", res.Output)
	}
	if !strings.Contains(res.Output, "BLOCKED:") {
		t.Fatalf("expected a clear refusal for the unexposed tool: %q", res.Output)
	}
	if len(called) != 1 || called[0] != "get_weather" {
		t.Fatalf("dispatcher saw %v, want only get_weather", called)
	}
}

// TestBridgeServesSeveralCallsOnOneConnection covers the case code mode exists
// for: one program making several tool calls in a single run.
func TestBridgeServesSeveralCallsOnOneConnection(t *testing.T) {
	requirePython(t)
	dir := scratch(t)

	bridge, err := Serve(dir, func(_ context.Context, name, args string) string {
		var a struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal([]byte(args), &a)
		return strings.Repeat("*", a.N)
	}, []string{"stars"})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer bridge.Close()
	if err := WriteShims(dir); err != nil {
		t.Fatalf("shims: %v", err)
	}

	script, _ := WriteScript(dir, "program.py", `
import agent_tools
total = "".join(agent_tools.call("stars", n=i) for i in range(1, 5))
print(len(total))
`)

	res := Run(context.Background(), Config{Dir: dir, Extra: bridge.Env()}, "python3", script)
	if !res.OK {
		t.Fatalf("run failed: %+v", res)
	}
	if strings.TrimSpace(res.Output) != "10" { // 1+2+3+4
		t.Fatalf("output = %q, want 10", res.Output)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestRelativeScratchDirWorks is a regression test for a bug every other test
// in this file was blind to.
//
// They all used t.TempDir(), which is absolute. Production passes
// ".harness/sandbox" — relative — and because the process runs WITH the scratch
// dir as its working directory, a relative script path was resolved against
// itself: ".harness/sandbox/run-7/.harness/sandbox/run-7/program.py". Every
// run_code call failed, and the error named the interpreter, so it read like a
// broken Python install rather than a path bug.
//
// The lesson is in the fixture, not the fix: a test helper that hands out
// absolute paths quietly tests a case production never has.
func TestRelativeScratchDirWorks(t *testing.T) {
	requirePython(t)

	// Run from a temp directory so the relative path lands somewhere disposable.
	t.Chdir(t.TempDir())

	dir, cleanup, err := Scratch("relative/scratch")
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	defer cleanup()

	if !filepath.IsAbs(dir) {
		t.Fatalf("Scratch must return an absolute path, got %q", dir)
	}

	script, err := WriteScript(dir, "program.py", "print('it ran')")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	res := Run(context.Background(), Config{Dir: "relative/scratch"}, "python3", script)
	if !res.OK {
		t.Fatalf("a relative scratch dir broke the run: %+v", res)
	}
	if !strings.Contains(res.Output, "it ran") {
		t.Fatalf("output = %q", res.Output)
	}
}
