// Package tools defines what the agent can DO. A Tool describes itself to the
// model and runs when the model asks for it.
//
// Nothing in here knows about the agent, the loop, or the terminal. That is the
// point of the package boundary: adding a capability means writing one struct
// and naming it in Default, and no other file changes.
package tools

import (
	"context"
	"encoding/json"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Tool is one capability the agent can invoke. Spec tells the model the tool
// exists and what arguments it takes; Run executes it with the JSON arguments
// the model produced.
//
// Two methods, because those are the two halves of a tool: what the model is
// told, and what actually happens. Keeping them on one type is what stops a
// description from drifting away from the code it describes.
type Tool interface {
	Spec() components.ChatFunctionTool
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
		name := specName(t.Spec())
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

// Specs returns every tool definition to advertise to the model. The order is
// stable between runs, which matters more than it looks: an unstable tool list
// changes the prompt, and a changing prompt quietly ruins prompt caching.
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

// Has reports whether the registry advertises a tool.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Dispatch runs the named tool and ALWAYS returns a string, turning any failure
// into text the model can read and react to rather than a crash. A tool that
// took the process down would take the conversation with it; a tool that says
// "that didn't work" lets the model try something else.
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

// specName pulls a function tool's advertised name out of the union type. Every
// tool here is a plain function tool, so that member is always populated.
func specName(spec components.ChatFunctionTool) string {
	return spec.ChatFunctionToolFunction.Function.Name
}

// defineTool builds a function-tool spec from a JSON-Schema string. The SDK
// wants parameters as a decoded object, so we unmarshal the schema here — the
// schemas are compile-time constants, so a parse failure is a programming error
// and we panic rather than surface it at runtime. Keeps each Spec() a one-liner.
func defineTool(name, description, schema string) components.ChatFunctionTool {
	var params map[string]any
	if err := json.Unmarshal([]byte(schema), &params); err != nil {
		panic("tools: invalid JSON schema for " + name + ": " + err.Error())
	}
	desc := description
	return components.CreateChatFunctionToolChatFunctionToolFunction(
		components.ChatFunctionToolFunction{
			Type: components.ChatFunctionToolTypeFunction,
			Function: components.ChatFunctionToolFunctionFunction{
				Name:        name,
				Description: &desc,
				Parameters:  params,
			},
		},
	)
}

// decode unmarshals the model's JSON arguments into a typed struct. Arguments
// are model output, so nothing in them is trusted to be well formed.
func decode(args string, into any) error {
	if args == "" {
		return nil
	}
	return json.Unmarshal([]byte(args), into)
}
