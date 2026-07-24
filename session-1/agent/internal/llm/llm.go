// Package llm builds the language-model client. It isolates the one place we
// point the OpenAI-compatible SDK at OpenRouter, so the rest of the app never
// needs to know the base URL.
package llm

import openai "github.com/sashabaranov/go-openai"

// NewOpenRouter returns an OpenAI-compatible client that talks to OpenRouter.
func NewOpenRouter(apiKey string) *openai.Client {
	config := openai.DefaultConfig(apiKey)
	config.BaseURL = "https://openrouter.ai/api/v1"
	return openai.NewClientWithConfig(config)
}
