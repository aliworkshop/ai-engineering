// Package tools defines what the agent can DO. A Tool describes itself to the
// model and runs when the model asks for it. Dangerous tools go through an
// Approver first — the human-in-the-loop gate.
package tools

import (
	"context"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/aliworkshop/ai-engineering-course/internal/toolspec"
)

// Approver is the human-in-the-loop gate. A dangerous tool must get a "yes"
// before it touches the system. The terminal UI implements this in production;
// tests supply a stub. Keeping it an interface means tools never import the UI.
type Approver interface {
	Confirm(action string) bool
}

// Tool is one capability the agent can invoke. Spec tells the model the tool
// exists and what arguments it takes; Run executes it with the JSON arguments
// the model produced.
type Tool interface {
	Spec() components.ChatFunctionTool
	Run(ctx context.Context, args string) (string, error)
}

// Sensitive marks a tool that changes something outside the agent — writes,
// deletes, shell commands. It is an optional interface rather than a field so
// a tool declares its own risk next to the code that takes it, and adding a
// tool cannot accidentally leave it off a list somewhere else.
//
// Two parts of the harness read it: the sandbox bridge, which refuses to expose
// anything sensitive to model-written code (Part 3), and the roster, which
// gives the generalist agent only the tools it needs and parks the rest with a
// specialist (Part 5).
type Sensitive interface {
	Sensitive() bool
}

// isSensitive reports a tool's own answer, defaulting to safe. Defaulting the
// other way sounds more cautious but is worse: every new read-only tool would
// silently vanish from the sandbox until someone remembered a method.
func isSensitive(t Tool) bool {
	s, ok := t.(Sensitive)
	return ok && s.Sensitive()
}

// Registry holds the agent's tools and routes calls to them by name.
type Registry struct {
	byName map[string]Tool
	order  []string // preserves a stable order when advertising tools
}

// NewRegistry indexes the given tools by their advertised name.
func NewRegistry(list ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(list))}
	for _, t := range list {
		name := specName(t.Spec())
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

// Specs returns every tool definition to advertise to the model.
func (r *Registry) Specs() []components.ChatFunctionTool {
	specs := make([]components.ChatFunctionTool, 0, len(r.order))
	for _, name := range r.order {
		specs = append(specs, r.byName[name].Spec())
	}
	return specs
}

// Names lists every tool in advertisement order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// SafeNames lists the tools that change nothing — the set sandboxed code is
// allowed to reach (Part 3).
func (r *Registry) SafeNames() []string {
	var out []string
	for _, name := range r.order {
		if !isSensitive(r.byName[name]) {
			out = append(out, name)
		}
	}
	return out
}

// Subset returns a registry holding only the named tools, in the order given.
// This is how least privilege is expressed in Part 5: an agent is a prompt plus
// a subset, and a tool a specialist doesn't hold is one it cannot be talked
// into using. Names that don't exist are skipped — a roster is configuration,
// and a typo in it should not take the process down at startup.
func (r *Registry) Subset(names ...string) *Registry {
	sub := &Registry{byName: make(map[string]Tool, len(names))}
	for _, name := range names {
		tool, ok := r.byName[name]
		if !ok {
			continue
		}
		sub.byName[name] = tool
		sub.order = append(sub.order, name)
	}
	return sub
}

// Has reports whether the registry advertises a tool.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// specName pulls a function tool's advertised name out of the union type. Every
// tool here is a plain function tool, so that member is always populated.
func specName(spec components.ChatFunctionTool) string {
	return spec.ChatFunctionToolFunction.Function.Name
}

// Dispatch runs the named tool and ALWAYS returns a string, turning any failure
// into text the model can read and react to rather than a crash.
func (r *Registry) Dispatch(ctx context.Context, name, args string) string {
	tool, ok := r.byName[name]
	if !ok {
		return "error: unknown tool " + name
	}
	result, err := tool.Run(ctx, args)
	if err != nil {
		return "error: " + err.Error()
	}
	return result
}

// defineTool and decode are the package-local spellings of the shared helpers.
// They live in toolspec so a tool in a sub-package this one imports — and which
// therefore can't import back — reaches the same code instead of a second copy
// of it.
func defineTool(name, description, schema string) components.ChatFunctionTool {
	return toolspec.Define(name, description, schema)
}

func decode(args string, into any) error {
	return toolspec.Decode(args, into)
}
