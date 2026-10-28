// Package sandbox is the one place model-written code runs.
//
// The door is the same one as before, and so is what comes back through it.
// What changed is what is on the other side: not an interpreter sharing this
// process, but a disposable container that an OpenSandbox server starts for
// one program and throws away afterwards. Its own filesystem, its own memory
// limit, no network, not root — and a clock that kills the process rather than
// asking it to stop.
//
// That move has a price, and it is worth seeing. A Go function cannot cross
// into another machine, so the world can no longer be a map of closures. It
// has to travel as data, with the few functions over it written in the
// language the program speaks. See World.
//
// It needs the server running — `docker compose up -d` — and
// OPEN_SANDBOX_API_KEY in .env.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// Result is what came back. It is JSON on purpose: the model reads it, so a
// failure has to be as legible as a success — a program that blew up should
// come back as something to fix, not as a wall.
type Result struct {
	OK    bool     `json:"ok"`
	Value any      `json:"result,omitempty"`
	Error string   `json:"error,omitempty"`
	Logs  []string `json:"logs"`
	Hint  string   `json:"hint,omitempty"`
}

// World is everything a program can reach.
//
// Data is encoded as JSON and frozen inside the container as `data`. Tools is
// a JavaScript expression over it that evaluates to the `tools` object. Both
// are written by us, never by the model, and nothing else is put in.
type World struct {
	Data  any
	Tools string
}

// The container. Small on purpose: arithmetic over a handful of charges needs
// none of the defaults' gigabytes, and a program that wants more is a program
// that should be stopped.
const (
	image  = "node:22-alpine"
	cpu    = "500m"
	memory = "256Mi"

	// node, in the official image — not root.
	user = int32(1000)

	// The container's own deadline, in case the Kill below never arrives:
	// a crash between create and kill should not leave one running forever.
	ttlSeconds = 120
)

// programPath is where the program is put. Read-only to the user it runs as,
// so it cannot rewrite itself.
const programPath = "/tmp/program.js"

// resultMark prefixes the one stdout line the runner writes. Anything else the
// program prints on its own is kept as a log, not mistaken for the answer.
const resultMark = "__sandbox_result__ "

// Box starts sandboxes. It holds the connection and nothing else, so it is
// safe to share and costs nothing to keep.
type Box struct {
	config opensandbox.ConnectionConfig
}

// New connects to the OpenSandbox server named by OPEN_SANDBOX_DOMAIN
// (default localhost:8080), using OPEN_SANDBOX_API_KEY.
func New() *Box {
	return &Box{config: opensandbox.ConnectionConfig{
		// The server runs in Docker and names sandboxes by the host it can
		// see. We are on the host, so that name is localhost.
		EndpointHostRewrite: map[string]string{"host.docker.internal": "localhost"},
		DisableMetrics:      true,
	}}
}

// Run executes code in a fresh container with world as the only thing it can
// reach, and throws the container away.
//
// timeout is the program's clock. Starting the container is not on it — that
// is the sandbox's cost, not the program's — but starting node is.
func (b *Box) Run(ctx context.Context, code string, world World, timeout time.Duration) Result {
	program, err := build(code, world)
	if err != nil {
		return Result{Error: err.Error(), Logs: []string{}}
	}

	ttl := ttlSeconds
	box, err := opensandbox.CreateSandbox(ctx, b.config, opensandbox.SandboxCreateOptions{
		Image:          image,
		ResourceLimits: opensandbox.ResourceLimits{"cpu": cpu, "memory": memory},
		TimeoutSeconds: &ttl,
		// Deny everything. There is no allow list because nothing in the
		// world needs the network — the data is already inside.
		NetworkPolicy: &opensandbox.NetworkPolicy{DefaultAction: "deny"},
	})
	if err != nil {
		// Said to the model as plainly as any other failure: it cannot fix
		// this, but it can stop retrying and say so.
		return Result{Error: "the sandbox could not be started: " + err.Error(), Logs: []string{}}
	}
	defer func() {
		// Its own context: the caller's may already be done, and a container
		// left behind is the one leak this package exists to prevent.
		kill, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = box.Kill(kill)
	}()

	err = box.UploadFile(ctx, strings.NewReader(program), opensandbox.UploadFileOptions{
		FileName: "program.js",
		Metadata: opensandbox.FileMetadata{Path: programPath, Mode: 444},
	})
	if err != nil {
		return Result{Error: "the program could not be copied in: " + err.Error(), Logs: []string{}}
	}

	uid := user
	started := time.Now()
	exec, err := box.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command: "node " + programPath,
		Timeout: timeout.Milliseconds(),
		UID:     &uid,
		GID:     &uid,
	}, nil)
	if err != nil {
		return Result{Error: "the program could not be run: " + err.Error(), Logs: []string{}}
	}
	return read(exec, time.Since(started), timeout)
}

// read turns what the container said into a Result.
func read(exec *opensandbox.Execution, elapsed, timeout time.Duration) Result {
	logs := []string{}
	var report string
	for _, line := range strings.Split(exec.Text(), "\n") {
		if rest, ok := strings.CutPrefix(line, resultMark); ok {
			report = rest
		} else if line != "" {
			logs = append(logs, line)
		}
	}

	if report != "" {
		var result Result
		if err := json.Unmarshal([]byte(report), &result); err != nil {
			return Result{Error: "the result could not be read back: " + err.Error(), Logs: logs}
		}
		// Whatever the program printed around console.log comes first,
		// because that is the order it happened in.
		result.Logs = append(logs, result.Logs...)
		return result
	}

	// No report means the process never reached the end of the runner. The
	// container killed it, or node itself fell over.
	if exec.Error != nil && strings.Contains(strings.Join(exec.Error.Traceback, " "), "killed") {
		if elapsed >= timeout {
			return Result{Error: "execution timed out after " + timeout.String(), Logs: logs}
		}
		// Killed before the clock ran out: the only other thing allowed to
		// kill it is the memory limit.
		return Result{Error: "the program was killed - most likely it ran out of memory (limit " + memory + ")", Logs: logs}
	}
	return Result{Error: "the program crashed: " + stderr(exec), Logs: logs}
}

func stderr(exec *opensandbox.Execution) string {
	var lines []string
	for _, m := range exec.Stderr {
		if text := strings.TrimSpace(m.Text); text != "" {
			lines = append(lines, text)
		}
	}
	if len(lines) == 0 && exec.Error != nil {
		return exec.Error.Name + ": " + exec.Error.Value
	}
	return strings.Join(lines, "\n")
}

// build writes the whole program: the world, the runner, and the model's code
// as a string the runner compiles.
//
// The code goes in as a JSON string handed to new Function, not pasted into
// the file. A syntax error then comes back as an ordinary error rather than
// node refusing to start, and nothing the model writes can reach outside its
// own function body to the runner around it.
func build(code string, world World) (string, error) {
	data, err := json.Marshal(world.Data)
	if err != nil {
		return "", fmt.Errorf("the world cannot be encoded as JSON: %w", err)
	}
	if strings.TrimSpace(world.Tools) == "" {
		return "", errors.New("the world has no tools")
	}
	source, err := json.Marshal(code)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(runner, resultMark, data, world.Tools, source), nil
}

// runner is the file the container runs. %s, in order: the result mark, the
// data as JSON, the tools expression, the model's code as a JSON string.
const runner = `"use strict";
(function () {
	const mark = %q;
	const logs = [];
	const show = (v) => {
		if (typeof v === "string") return v;
		try { return JSON.stringify(v); } catch (e) { return String(v); }
	};
	console.log = (...args) => { logs.push(args.map(show).join(" ")); };

	const freeze = (v) => {
		if (v && typeof v === "object") { Object.values(v).forEach(freeze); Object.freeze(v); }
		return v;
	};
	const data = freeze(%s);
	const tools = Object.freeze(%s);

	const report = (r) => { process.stdout.write("\n" + mark + JSON.stringify(r) + "\n"); };

	let value;
	try {
		// Wrapped in a function so the code can return a result, which is
		// the only way it has of handing anything back.
		value = new Function("tools", %s)(tools);
	} catch (e) {
		report({ ok: false, error: e instanceof Error ? e.name + ": " + e.message : String(e), logs });
		return;
	}

	if (value === undefined || value === null) {
		// A run that returns nothing reads as silence. Say what to do about
		// it, because the model cannot see why its answer vanished.
		report({ ok: true, logs, hint: "your code returned no value - end with ` + "`return <result>`" + `" });
		return;
	}

	// JSON.stringify writes NaN as null without a word, so a misspelled field
	// turns into a confident wrong answer. Refuse it here, as a sentence the
	// model can act on.
	let bad = "";
	JSON.stringify(value, (k, v) => {
		if (typeof v === "number" && !Number.isFinite(v)) bad = bad || (k || "the result") + " is " + v;
		return v;
	});
	if (bad) {
		report({ ok: false, error: "the result cannot be encoded as JSON: " + bad, logs });
		return;
	}
	report({ ok: true, result: value, logs });
})();
`
