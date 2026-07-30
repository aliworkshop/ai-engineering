// Package llm builds the language-model client. It isolates the one place we
// construct the OpenRouter SDK client, so the rest of the app never needs to
// know how it's wired.
package llm

import openrouter "github.com/OpenRouterTeam/go-sdk"

// NewOpenRouter returns an OpenRouter SDK client authenticated with the given
// API key. The SDK targets OpenRouter's production endpoint by default.
func NewOpenRouter(apiKey string) *openrouter.OpenRouter {
	return openrouter.New(openrouter.WithSecurity(apiKey))
}
