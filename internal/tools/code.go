package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/aliworkshop/ai-engineering-course/internal/sandbox"
)

// RunCode is code mode: the model writes one program, the program runs inside
// the sandbox, and while it runs it can call back into the agent's read-only
// tools through a socket bridge.
//
// It is not gated by the Approver, and that is the deal it strikes: it may run
// arbitrary code, but only with tools that cannot change anything, in a
// throwaway directory, with the environment stripped and a timer running. The
// tools that *do* change things stay on the file tools, where a human still
// sees them one at a time.
type RunCode struct {
	// Scratch is where each run's disposable directory is created.
	Scratch string

	// Dispatch runs one of the agent's tools. It is the Registry's own method,
	// so sandboxed code and the model reach identical implementations.
	Dispatch func(ctx context.Context, name, args string) string

	// Expose lists the tool names the sandboxed program may call. Read-only
	// ones only — Default fills this in from the registry.
	Expose []string
}

func (t RunCode) Spec() components.ChatFunctionTool {
	// The exposed tool names are listed in the description rather than left for
	// the model to guess, and they are read from t.Expose at advertise time so
	// the list cannot drift from what the bridge actually allows.
	//
	// The blunt sentence about the scratch directory is there because of an
	// observed failure: handed a sandbox, a model's first instinct is
	// open("data.txt") — which fails with a FileNotFoundError naming a temp path
	// it has never heard of, and it then burns two more turns trying variations
	// of the same wrong idea. Saying "your cwd is empty, everything outside it
	// comes from a tool" up front costs nine words and saves the whole detour.
	available := "none"
	if len(t.Expose) > 0 {
		available = strings.Join(t.Expose, ", ")
	}

	return defineTool("run_code",
		"Write and run a short program (python or bash) in a sandbox, and return its output. "+
			"Prefer this over several tool calls when a task needs looking something up and then "+
			"filtering, counting, or calculating over the result: write ONE program that does the "+
			"whole job.\n"+
			"The sandbox starts in an EMPTY throwaway directory and cannot see the user's files, "+
			"the network, or any credentials. Anything from outside must come through the tool "+
			"bridge — open() and requests will not reach it.\n"+
			"In python:  import agent_tools; text = agent_tools.call(\"get_weather\", location=\"Tokyo\")\n"+
			"In bash:    ./tools get_weather '{\"location\":\"Tokyo\"}'\n"+
			"Tools available inside the sandbox: "+available+".\n"+
			"Print what you want to see — stdout is the result.",
		`{"type":"object","properties":{`+
			`"language":{"type":"string","enum":["python","bash"]},`+
			`"code":{"type":"string"}},"required":["language","code"]}`)
}

func (t RunCode) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Code) == "" {
		return "", fmt.Errorf("run_code needs a non-empty code argument")
	}

	interpreter, filename, err := runtimeFor(a.Language)
	if err != nil {
		return "", err
	}

	// One scratch dir per run: the socket, the shims, and the model's program
	// all live in it, and all of it is deleted afterwards.
	dir, cleanup, err := sandbox.Scratch(t.Scratch)
	if err != nil {
		return "", err
	}
	defer cleanup()

	script, err := sandbox.WriteScript(dir, filename, a.Code)
	if err != nil {
		return "", err
	}

	cfg := sandbox.Config{Dir: dir}

	// The bridge is optional: without it the sandbox still runs code, it just
	// has no tools to call. Refusing the whole call because the bridge could not
	// start would be worse than running with less — but the model must be TOLD,
	// or it spends its next three turns debugging an ImportError it cannot fix.
	var bridgeErr string
	if t.Dispatch != nil && len(t.Expose) > 0 {
		bridge, err := sandbox.Serve(dir, t.Dispatch, t.Expose)
		switch {
		case err != nil:
			bridgeErr = "the tool bridge is unavailable in this run (" + err.Error() +
				"); agent_tools cannot be imported, so solve this without calling tools"
		default:
			defer bridge.Close()
			cfg.Extra = bridge.Env()
			if err := sandbox.WriteShims(dir); err != nil {
				return "", err
			}
		}
	}

	result := sandbox.Run(ctx, cfg, interpreter, script)

	// Returned as JSON rather than prose because the model has to distinguish
	// "my program printed nothing" from "my program crashed" from "it ran too
	// long" — and act differently on each.
	out, err := json.Marshal(struct {
		sandbox.Result
		ToolsError string `json:"tools_error,omitempty"`
		Hint       string `json:"hint,omitempty"`
	}{Result: result, ToolsError: bridgeErr, Hint: hintFor(result, t.Expose)})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// hintFor names the one mistake models reliably make here.
//
// Given a sandbox, a model's first instinct is open("go.mod") — and the
// resulting FileNotFoundError names a temp path it has never heard of, so it
// concludes the file does not exist and reports a confident zero rather than
// reaching for the bridge. Smaller models do this repeatedly even when the tool
// description says not to.
//
// The harness knows exactly what went wrong here, so it says so. Correcting a
// known failure mode at the point of failure is worth more than another
// sentence of prompt nobody reads: it costs one string comparison and turns a
// three-turn spiral into one retry.
func hintFor(result sandbox.Result, expose []string) string {
	if result.OK || len(expose) == 0 {
		return ""
	}
	if !strings.Contains(result.Output, "FileNotFoundError") &&
		!strings.Contains(result.Output, "No such file or directory") {
		return ""
	}
	return "The sandbox directory is empty — it cannot see the user's files or the network. " +
		"Anything from outside has to come through the bridge instead: " +
		"agent_tools.call(\"<tool>\", ...) in python, or ./tools <tool> '{...}' in bash. " +
		"Available: " + strings.Join(expose, ", ") + "."
}

// runtimeFor maps the advertised languages onto an interpreter. Keeping the
// list closed — rather than running whatever the model names — means a typo
// gets a useful error instead of executing something unexpected.
func runtimeFor(language string) (interpreter, filename string, err error) {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "python", "python3", "py":
		return "python3", "program.py", nil
	case "bash", "sh", "shell":
		return "bash", "program.sh", nil
	default:
		return "", "", fmt.Errorf("run_code supports language \"python\" or \"bash\", not %q", language)
	}
}
