// Package sandbox is the one place model-written code runs.
//
// There is a better reason to care about this than "block the bad stuff".
// Chaining tools for anything analytical means every intermediate result round
// trips through the model — read the file, model reads it, filter, model reads
// that, total it — which is slow, expensive, and squarely where LLMs are
// weakest. Code mode flips it: hand the model the tools as an API and let it
// write one program that does the whole job. Models have read millions of lines
// of real code and only contrived tool-call examples, so on multi-step work
// this takes markedly fewer turns.
//
// And that is exactly why the sandbox has to exist. Code mode is the reason;
// mediation is the price.
//
// Honest limits, stated up front: a child process with a killed process group,
// a scrubbed environment, a scratch directory and a wall-clock timeout stops
// accidents, runaway loops, and casual credential exfiltration. It is not a
// security boundary against a determined attacker — that needs a container or
// a disposable micro-VM. What the harness owes you either way is the same, and
// is the actual lesson: every dangerous capability goes through ONE mediated
// door, so hardening later means changing this file and nothing else.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Defaults chosen to be visibly finite. Model-written code that needs more than
// a few seconds or produces more than a few pages is nearly always a mistake
// you want reported, not waited on.
const (
	DefaultTimeout   = 20 * time.Second
	DefaultMaxOutput = 16 << 10 // 16 KiB
)

// safeEnv is the entire environment a sandboxed process inherits. Everything
// else — every API key in your .env, every token in your shell — is left
// behind. This is the cheapest, highest-value thing in the package: a program
// that cannot see a credential cannot leak one, however it was written.
var safeEnv = []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR"}

// Config describes the little world a process is given.
type Config struct {
	// Dir is the working directory. Code is run with this as its cwd, so
	// relative paths land somewhere disposable rather than in your repo.
	Dir string

	// Timeout bounds wall-clock time. Expired means the whole process group is
	// killed, so a backgrounded child cannot outlive the run that spawned it.
	Timeout time.Duration

	// MaxOutput caps captured bytes. A program that prints forever should
	// produce a truncated result and a clear error, not an OOM.
	MaxOutput int

	// Extra adds environment variables on top of the allowlist — how the tool
	// bridge tells sandboxed code where to find its socket.
	Extra []string
}

func (c Config) normalized() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}
	if c.Dir == "" {
		c.Dir = os.TempDir()
	}
	// Absolute for the same reason Scratch is: cmd.Dir and any path argument
	// must not be resolved against each other. See Scratch.
	if absolute, err := filepath.Abs(c.Dir); err == nil {
		c.Dir = absolute
	}
	return c
}

// Result is what came back. It is a value, not an error, because "the code the
// model wrote blew up" is information the model should get and reason about —
// not a failure of the harness.
type Result struct {
	OK       bool   `json:"ok"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Millis   int64  `json:"millis"`
}

// Run executes one command inside the sandbox and always returns a Result.
func Run(ctx context.Context, cfg Config, name string, args ...string) Result {
	cfg = cfg.normalized()

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return Result{Error: "sandbox: prepare dir: " + err.Error()}
	}

	// The timeout is our own, not the caller's, so a hung program dies on our
	// schedule even when the caller passed a context that never expires.
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = append(inherit(), cfg.Extra...)
	isolateProcessGroup(cmd)

	// Stdin is /dev/null: a program that tries to prompt should get EOF and
	// exit, not block until the timeout fires.
	cmd.Stdin = strings.NewReader("")

	var buf bytes.Buffer
	capped := &cappedWriter{w: &buf, limit: cfg.MaxOutput}
	cmd.Stdout, cmd.Stderr = capped, capped

	// Cancel alone only signals the immediate child. Killing the whole group is
	// what stops `python -c '...' & disown`-shaped escapes from surviving.
	cmd.Cancel = func() error { return killGroup(cmd) }

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := Result{Output: buf.String(), Millis: elapsed.Milliseconds()}
	if capped.truncated {
		res.Output += fmt.Sprintf("\n…(output truncated at %d bytes)", cfg.MaxOutput)
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.Error = fmt.Sprintf("execution timed out after %s", cfg.Timeout)
	case err != nil:
		res.Error = err.Error()
	default:
		res.OK = true
	}
	if strings.TrimSpace(res.Output) == "" && res.OK {
		res.Output = "(no output)"
	}
	return res
}

// inherit copies through only the allowlisted variables.
func inherit() []string {
	out := make([]string, 0, len(safeEnv))
	for _, key := range safeEnv {
		if v, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+v)
		}
	}
	return out
}

// Scratch makes a fresh disposable directory under base. Each run gets its own
// so one program cannot read what the last one left behind, and so cleaning up
// is a single RemoveAll.
//
// The returned path is always ABSOLUTE, and that is not cosmetic. Every path
// this package hands out is later used as an argument to a process whose
// working directory is the scratch dir itself — so a relative path gets
// resolved against the very directory it already names, and you get
// ".harness/sandbox/run-7/.harness/sandbox/run-7/program.py". The failure looks
// like the interpreter is broken rather than like a path bug, so it is worth
// making structurally impossible here instead of correct-by-convention at four
// call sites.
func Scratch(base string) (dir string, cleanup func(), err error) {
	absolute, err := filepath.Abs(base)
	if err != nil {
		return "", func() {}, fmt.Errorf("sandbox: resolve scratch base: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("sandbox: create scratch base: %w", err)
	}
	dir, err = os.MkdirTemp(absolute, "run-")
	if err != nil {
		return "", func() {}, fmt.Errorf("sandbox: create scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// WriteScript drops the model's source into the scratch dir under a name the
// interpreter will accept.
func WriteScript(dir, filename, source string) (string, error) {
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		return "", fmt.Errorf("sandbox: write script: %w", err)
	}
	return path, nil
}

// cappedWriter stops collecting past a limit but keeps accepting writes, so a
// chatty program is truncated rather than killed by a short write it would
// report as a broken pipe.
type cappedWriter struct {
	w         *bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	room := c.limit - c.w.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.w.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.w.Write(p)
}
