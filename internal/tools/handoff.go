package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Handoff transfers control to a specialist agent.
//
// The honest framing first: usually you do not need this. A capable model with
// good tools is a generalist, and reaching for a swarm of agents is normally
// over-engineering. A handoff earns its keep when the other agent is genuinely
// a different thing — and the case that always qualifies is least privilege.
// The generalist here cannot write, delete, or run commands, because it does
// not hold those tools at all. It can explain what needs doing and hand over;
// it cannot be prompted into doing it, because there is nothing to call.
//
// The tool itself does almost nothing: it validates the destination and returns
// a marker. The transfer is performed by the harness loop, which is the only
// thing that can swap out the running agent — the same ownership rule that lets
// the harness checkpoint tool calls and gate dangerous ones.
type Handoff struct {
	// To lists the specialists that exist, so a model that invents a name gets
	// a correctable error instead of a silent no-op.
	To map[string]string // name -> what it is for
}

// HandoffResult is what the harness looks for after a handoff call. It travels
// back as the tool result so the transfer shows up in the transcript the model
// sees, not just in harness-internal state.
type HandoffResult struct {
	Handoff bool   `json:"handoff"`
	To      string `json:"to"`
	Reason  string `json:"reason,omitempty"`
}

// HandoffTool is the advertised name the harness intercepts.
const HandoffTool = "handoff"

func (t Handoff) Spec() components.ChatFunctionTool {
	var lines []string
	for name, purpose := range t.To {
		lines = append(lines, fmt.Sprintf("%q: %s", name, purpose))
	}
	// Sorted so the prompt the model sees is stable between runs — an unstable
	// prompt quietly ruins prompt caching and makes evals non-reproducible.
	sortStrings(lines)

	return defineTool(HandoffTool,
		"Hand the conversation to a specialist agent that holds tools you do not. "+
			"The specialist takes over and finishes the task; do not also attempt the work "+
			"yourself. Available: "+strings.Join(lines, "; "),
		`{"type":"object","properties":{`+
			`"to":{"type":"string"},`+
			`"reason":{"type":"string","description":"what the specialist needs to do, in one sentence"}},`+
			`"required":["to","reason"]}`)
}

func (t Handoff) Run(_ context.Context, args string) (string, error) {
	var a struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	target := strings.TrimSpace(a.To)
	if _, ok := t.To[target]; !ok {
		var known []string
		for name := range t.To {
			known = append(known, name)
		}
		sortStrings(known)
		return "", fmt.Errorf("no agent named %q; available: %s", target, strings.Join(known, ", "))
	}

	out, err := json.Marshal(HandoffResult{Handoff: true, To: target, Reason: a.Reason})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sortStrings is an insertion sort to keep this file free of an import that
// exists solely to order two or three lines.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
