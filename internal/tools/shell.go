package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// RunCommand runs a shell command and returns its combined output. This is how
// the agent executes the scripts it writes. Dangerous — gated by the Approver.
type RunCommand struct{ Approver Approver }

func (RunCommand) Spec() components.ChatFunctionTool {
	return defineTool("run_command",
		"Run a shell command and return its combined output. Use this to execute scripts. Requires human approval.",
		`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)
}

func (t RunCommand) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if !t.Approver.Confirm(fmt.Sprintf("RUN command: %s", a.Command)) {
		return "Denied by user; command not run.", nil
	}

	out, err := exec.CommandContext(ctx, "sh", "-c", a.Command).CombinedOutput()
	result := string(out)
	if err != nil {
		result += "\n(exit error: " + err.Error() + ")"
	}
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	return result, nil
}
