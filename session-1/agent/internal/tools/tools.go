// Package tools defines what the agent can DO. A Tool describes itself to the
// model and runs when the model asks for it. Dangerous tools go through an
// Approver first — the human-in-the-loop gate.
package tools

import (
	"context"
	"encoding/json"

	openai "github.com/sashabaranov/go-openai"
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
	Spec() openai.Tool
	Run(ctx context.Context, args string) (string, error)
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
		name := t.Spec().Function.Name
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

// Specs returns every tool definition to advertise to the model.
func (r *Registry) Specs() []openai.Tool {
	specs := make([]openai.Tool, 0, len(r.order))
	for _, name := range r.order {
		specs = append(specs, r.byName[name].Spec())
	}
	return specs
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

// defineTool builds an openai.Tool from a JSON-Schema string. Small helper so
// each tool's Spec() stays a one-liner.
func defineTool(name, description, schema string) openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(schema),
		},
	}
}

// decode unmarshals the model's JSON arguments into a typed struct.
func decode(args string, into any) error {
	if args == "" {
		return nil
	}
	return json.Unmarshal([]byte(args), into)
}
