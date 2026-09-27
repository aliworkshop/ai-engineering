package sandbox

import (
	"strings"
	"testing"
	"time"
)

// api is a small world to run against: one function, and nothing else.
func api() map[string]any {
	return map[string]any{
		"double": func(n int) int { return n * 2 },
	}
}

func run(code string) Result { return Run(code, api(), 2*time.Second) }

func TestReturnsAValue(t *testing.T) {
	got := run(`return tools.double(21)`)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != int64(42) {
		t.Errorf("result = %#v, want 42", got.Value)
	}
}

// The timeout is the only thing that stops a loop that never yields.
func TestTimeoutKillsARunawayLoop(t *testing.T) {
	started := time.Now()
	got := Run("while (true) {}", api(), 50*time.Millisecond)

	if got.OK {
		t.Fatal("an infinite loop returned successfully")
	}
	if !strings.Contains(got.Error, "timed out") {
		t.Errorf("error = %q, want it to say it timed out", got.Error)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("took %s to give up, want about the timeout", elapsed)
	}
}

// Nothing of the host is reachable, and not because it was blocked: it was
// never put in the world.
func TestTheHostIsNotInTheWorld(t *testing.T) {
	for _, name := range []string{"require", "process", "fetch", "globalThis.fs"} {
		got := run("return typeof " + name)
		if got.OK && got.Value == "undefined" {
			continue
		}
		if !got.OK && strings.Contains(got.Error, "not defined") {
			continue
		}
		t.Errorf("%s is reachable: ok=%v value=%#v error=%s", name, got.OK, got.Value, got.Error)
	}
}

// A bug comes back as something the model can read and fix, not as a crash.
func TestABugIsAStructuredError(t *testing.T) {
	got := run("return totallyUndefined.value")
	if got.OK {
		t.Fatal("a reference error was reported as success")
	}
	if !strings.Contains(got.Error, "totallyUndefined") {
		t.Errorf("error = %q, want it to name what was missing", got.Error)
	}
}

func TestConsoleLogIsCaptured(t *testing.T) {
	got := run(`console.log("charges", 3); console.log("done"); return 1`)
	want := []string{"charges 3", "done"}
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
	got := run(`tools.double(1);`)
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
	got := run("return { refund: 0 / 0 }")
	if got.OK {
		t.Fatal("NaN came back as a successful result")
	}
	if !strings.Contains(got.Error, "JSON") {
		t.Errorf("error = %q, want it to say the result could not be encoded", got.Error)
	}
}

// Go structs reach the code under their JSON names, which is what the runCode
// tool description promises the model. Lose the mapping and c.amount is
// undefined, arithmetic on it is NaN, and the answer disappears without
// anything saying why — which is exactly what happened before this existed.
func TestGoStructFieldsUseTheirJSONNames(t *testing.T) {
	type charge struct {
		ID     string `json:"id"`
		Amount int    `json:"amount"`
	}
	world := map[string]any{
		"getCharges": func() []charge { return []charge{{ID: "ch_001", Amount: 4900}} },
	}

	got := Run(`const c = tools.getCharges()[0]; return c.id + "|" + (c.amount / 100)`,
		world, 2*time.Second)
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}
	if got.Value != "ch_001|49" {
		t.Errorf("result = %#v, want \"ch_001|49\"", got.Value)
	}
}
