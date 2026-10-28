package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

// world is a small world to run against: one function, and nothing else.
func world() World {
	return World{
		Data:  map[string]any{"factor": 2},
		Tools: `{ double: (n) => n * data.factor }`,
	}
}

var (
	reachable     bool
	reachableOnce sync.Once
)

// live returns a Box, or skips: these tests start real containers, and a
// laptop without the server running should get a sentence, not a failure.
func live(t *testing.T) *Box {
	t.Helper()
	_ = godotenv.Load("../../.env")
	box := New()
	reachableOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reachable = !strings.Contains(box.Run(ctx, "return 1", world(), 2*time.Second).Error, "could not be started")
	})
	if !reachable {
		t.Skip("no OpenSandbox server - run `docker compose up -d` and set OPEN_SANDBOX_API_KEY")
	}
	return box
}

func run(t *testing.T, code string) Result {
	return live(t).Run(context.Background(), code, world(), 2*time.Second)
}

func TestReturnsAValue(t *testing.T) {
	got := run(t, `return tools.double(21)`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != float64(42) {
		t.Errorf("result = %#v, want 42", got.Value)
	}
}

// The timeout is the only thing that stops a loop that never yields.
func TestTimeoutKillsARunawayLoop(t *testing.T) {
	box := live(t)
	started := time.Now()
	got := box.Run(context.Background(), "while (true) {}", world(), 500*time.Millisecond)

	if got.OK {
		t.Fatal("an infinite loop returned successfully")
	}
	if !strings.Contains(got.Error, "timed out") {
		t.Errorf("error = %q, want it to say it timed out", got.Error)
	}
	// Starting a container is most of this; the bound is that it ends.
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("took %s to give up, want about the timeout plus a container", elapsed)
	}
}

// Nothing of the host is reachable. The program has a filesystem and a
// process, and they are the container's: this machine's home directories are
// not in it, and the network is not there at all.
func TestTheHostIsNotInTheWorld(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, `
		const fs = process.mainModule.require("fs");
		return { home: fs.existsSync(`+strconv.Quote(home)+`), uid: process.getuid() };
	`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	seen := got.Value.(map[string]any)
	if seen["home"] != false {
		t.Errorf("the host's home directories are visible: %v", seen)
	}
	if seen["uid"] == float64(0) {
		t.Errorf("the program runs as root")
	}
}

func TestTheNetworkIsDenied(t *testing.T) {
	got := live(t).Run(context.Background(), `
		return process.mainModule.require("child_process")
			.execSync("node -e \"fetch('https://example.com').then(() => console.log('reached'), () => console.log('blocked'))\"")
			.toString().trim();
	`, world(), 10*time.Second)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != "blocked" {
		t.Errorf("fetch from inside = %#v, want it blocked", got.Value)
	}
}

// The memory limit kills a hog, and the error says which limit it hit rather
// than blaming the clock.
func TestAMemoryHogIsKilled(t *testing.T) {
	got := live(t).Run(context.Background(),
		"const a = []; while (true) a.push(Buffer.alloc(1e7, 1));", world(), 20*time.Second)
	if got.OK {
		t.Fatal("a program that ate all the memory returned successfully")
	}
	if !strings.Contains(got.Error, "memory") {
		t.Errorf("error = %q, want it to say it ran out of memory", got.Error)
	}
}

// A syntax error is a bug like any other, not node refusing to start.
func TestASyntaxErrorIsAStructuredError(t *testing.T) {
	got := run(t, "return {")
	if got.OK {
		t.Fatal("a syntax error was reported as success")
	}
	if !strings.HasPrefix(got.Error, "SyntaxError") {
		t.Errorf("error = %q, want a SyntaxError", got.Error)
	}
}

// A bug comes back as something the model can read and fix, not as a crash.
func TestABugIsAStructuredError(t *testing.T) {
	got := run(t, "return totallyUndefined.value")
	if got.OK {
		t.Fatal("a reference error was reported as success")
	}
	if !strings.Contains(got.Error, "totallyUndefined") {
		t.Errorf("error = %q, want it to name what was missing", got.Error)
	}
}

func TestConsoleLogIsCaptured(t *testing.T) {
	got := run(t, `console.log("charges", 3, {id: "ch_001"}); console.log("done"); return 1`)
	want := []string{`charges 3 {"id":"ch_001"}`, "done"}
	if len(got.Logs) != len(want) {
		t.Fatalf("logs = %v, want %v", got.Logs, want)
	}
	for i := range want {
		if got.Logs[i] != want[i] {
			t.Errorf("log %d = %q, want %q", i, got.Logs[i], want[i])
		}
	}
}

// Silence is the failure nobody reports, so it gets a nudge instead.
func TestNoReturnValueGetsAHint(t *testing.T) {
	got := run(t, `tools.double(1);`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != nil {
		t.Errorf("result = %#v, want nothing", got.Value)
	}
	if !strings.Contains(got.Hint, "return") {
		t.Errorf("hint = %q, want it to say to return something", got.Hint)
	}
}

// The result has to survive being turned into JSON, because that is how the
// model reads it. NaN does not, and an unreadable answer is worse than an
// error — this is the one that was caught by running it.
func TestAResultThatCannotBeJSONIsAnError(t *testing.T) {
	got := run(t, "return { refund: 0 / 0 }")
	if got.OK {
		t.Fatal("NaN came back as a successful result")
	}
	if !strings.Contains(got.Error, "JSON") {
		t.Errorf("error = %q, want it to say the result could not be encoded", got.Error)
	}
}

// The world is data, and the code cannot change it: a program that "fixes" a
// charge has fixed nothing, and a later read sees the same thing.
func TestTheWorldIsReadOnly(t *testing.T) {
	got := run(t, `
		"use strict";
		try { data.factor = 100; } catch (e) {}
		try { tools.double = () => 0; } catch (e) {}
		return tools.double(1);
	`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != float64(2) {
		t.Errorf("result = %#v, want 2 - the world was changed", got.Value)
	}
}

// What the program writes to stdout by itself is kept as a log, and cannot
// pass itself off as the answer.
func TestStdoutIsALogNotTheAnswer(t *testing.T) {
	got := run(t, `process.stdout.write("hello\n"); return "answer"`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != "answer" || len(got.Logs) != 1 || got.Logs[0] != "hello" {
		t.Errorf("result = %#v, logs = %q", got.Value, got.Logs)
	}
}

// A world that cannot be written down cannot be sent, and that is caught
// before any container is started.
func TestAWorldThatIsNotJSONIsRefused(t *testing.T) {
	got := New().Run(context.Background(), "return 1",
		World{Data: map[string]any{"f": func() {}}, Tools: "{}"}, time.Second)
	if got.OK || !strings.Contains(got.Error, "JSON") {
		t.Errorf("got %+v, want an error about JSON", got)
	}
}
