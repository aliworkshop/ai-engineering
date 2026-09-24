package tools

import openrouter "github.com/OpenRouterTeam/go-sdk"

// Default builds the standard toolset. This is the single place that decides
// which tools the agent has — main wires the client, this file decides what to
// do with it, and the agent finds out by asking the registry.
func Default(client *openrouter.OpenRouter) *Registry {
	return NewRegistry(
		WebSearch{Client: client, Model: SearchModel},
	)
}
