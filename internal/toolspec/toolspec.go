// Package toolspec is the plumbing a tool needs to describe itself to the model
// and to read back the arguments the model produced.
//
// It exists as its own package because tools now live in more than one place:
// the tools package holds the general set, and tools/diagram holds the drawing
// tools. Since tools imports tools/diagram to register them, tools/diagram
// cannot import tools back — so the pieces both need sit here, in a leaf that
// imports nothing of ours.
package toolspec

import (
	"encoding/json"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Define builds a function-tool spec from a JSON-Schema string. The SDK wants
// parameters as a decoded object, so we unmarshal the schema here — the schemas
// are compile-time constants, so a parse failure is a programming error and we
// panic rather than surface it at runtime. Keeps each Spec() a one-liner.
func Define(name, description, schema string) components.ChatFunctionTool {
	var params map[string]any
	if err := json.Unmarshal([]byte(schema), &params); err != nil {
		panic("toolspec: invalid JSON schema for " + name + ": " + err.Error())
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

// Decode unmarshals the model's JSON arguments into a typed struct.
func Decode(args string, into any) error {
	if args == "" {
		return nil
	}
	return json.Unmarshal([]byte(args), into)
}
