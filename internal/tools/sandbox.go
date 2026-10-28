package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/OpenRouterTeam/go-sdk/models/components"

	"github.com/aliworkshop/ai-engineering-course/internal/sandbox"
)

// RunCodeTool is the advertised name.
const RunCodeTool = "runCode"

// codeTimeout is how long a program gets, counted from starting node — not
// from starting the container, which is the sandbox's cost and not the
// program's. Two seconds is generous for arithmetic over a handful of charges
// and short enough that a runaway loop is noticed rather than waited on.
const codeTimeout = 2 * time.Second

// Charge is one line on a customer's statement. Amounts are in cents, which is
// how every payment system stores money and how the model will have to handle
// it — the reply says $49.00, the data says 4900, and something has to divide.
type Charge struct {
	ID          string `json:"id"`
	Amount      int    `json:"amount"`
	Date        string `json:"date"`
	Description string `json:"description"`
}

// Charges is the canned billing data. Note ch_001 and ch_002: same amount,
// same day, same description. The duplicate is planted, because the scenario
// this part exists for is "find it and work out the refund" — three tool calls
// and some mental arithmetic in the old toolbox, one small program in this one.
var Charges = map[string][]Charge{
	"cus_88121": {
		{ID: "ch_001", Amount: 4900, Date: "2026-05-01", Description: "Pro plan - monthly"},
		{ID: "ch_002", Amount: 4900, Date: "2026-05-01", Description: "Pro plan - monthly"},
		{ID: "ch_003", Amount: 1500, Date: "2026-04-18", Description: "Extra seats"},
	},
}

// SandboxWorld is the whole world a program can reach.
//
// Both functions only READ, and that is the rule that makes the rest of this
// safe to build. A step is replayed after a crash, and replaying a lookup costs
// nothing; replaying a refund costs a customer. So sendReply stays outside,
// an ordinary tool with an ordinary exactly-once checkpoint, and nothing that
// changes the world is ever put in here.
//
// The program runs in another container, and a Go function cannot follow it
// there. So the world goes in as data — the charges and the knowledge base —
// with the two functions over it written in JavaScript. searchKnowledgeBase is
// searchKB again, line for line; TestSandboxSearchMatchesSearchKB holds them
// to the same answers.
func SandboxWorld() sandbox.World {
	return sandbox.World{
		Data: map[string]any{
			"charges":       Charges,
			"knowledgeBase": KnowledgeBase,
			"miss":          kbMiss,
		},
		Tools: `{
			getCharges: (customerId) => data.charges[customerId] || [],
			searchKnowledgeBase: (query) => {
				const q = String(query).toLowerCase();
				const hits = Object.keys(data.knowledgeBase)
					.filter((topic) => q.includes(topic))
					.sort()
					.map((topic) => data.knowledgeBase[topic]);
				return hits.length ? hits : [data.miss];
			},
		}`,
	}
}

// RunCode runs a program the model wrote.
//
// The tool the rest of the harness has been building towards: instead of a
// chain of chatty calls with the arithmetic done in the model's head, one
// program fetches, groups, compares and computes, and hands back an answer.
// What makes that affordable is that it runs behind one door.
type RunCode struct {
	Box *sandbox.Box
}

func (RunCode) Spec() components.ChatFunctionTool {
	return defineTool(RunCodeTool,
		"Run a JavaScript program to fetch and analyze data. Available inside:\n"+
			"  tools.getCharges(customerId) -> [{id, amount (in cents), date, description}]\n"+
			"  tools.searchKnowledgeBase(query) -> [string]\n"+
			"  console.log(...) for debugging\n"+
			"These are SYNCHRONOUS - do not use await.\n"+
			"IMPORTANT: the code MUST end with a `return` statement (any JSON value) -\n"+
			"  e.g. `return { duplicateId, refund }` - otherwise the result is lost.",
		`{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}`)
}

func (r RunCode) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Code string `json:"code"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Code) == "" {
		return "", fmt.Errorf("code is required")
	}
	return marshal(r.Box.Run(ctx, a.Code, SandboxWorld(), codeTimeout))
}
