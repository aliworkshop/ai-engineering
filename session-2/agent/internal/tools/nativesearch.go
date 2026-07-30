package tools

import (
	"context"
	"fmt"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// NativeWebSearch searches the web through OpenRouter itself rather than a
// third-party search API. OpenRouter exposes search as a *plugin* on a chat
// request: we send the query as a one-off completion with the "web" plugin
// attached, OpenRouter runs the search, injects the hits into that request's
// prompt, and the model writes the findings back to us. So unlike WebSearch —
// which returns raw ranked results — this returns an already-digested answer
// with source URLs, at the cost of one extra model call.
//
// Read-only, so no approval is needed.
type NativeWebSearch struct {
	// Client is the authenticated OpenRouter client used for the search call.
	Client *openrouter.OpenRouter
	// Model runs the search request. Any model works — the plugin does the
	// searching — so a small, cheap one is the sensible choice.
	Model string
	// MaxResults caps how many hits OpenRouter feeds into the prompt.
	// Zero means defaultMaxResults.
	MaxResults int64
	// Engine optionally forces a search backend ("native", "exa", "parallel",
	// "firecrawl", "perplexity"). Empty lets OpenRouter choose.
	Engine components.WebSearchEngine
}

// defaultMaxResults keeps the injected search context small enough to stay cheap
// while still covering the usual "what's the current X?" question.
const defaultMaxResults = 5

// nativeSearchPrompt shapes the sub-request's answer into something the *outer*
// agent can quote: facts plus their sources, no conversational padding. The
// citation instruction matters — the chat API surfaces the plugin's citations as
// annotations that this SDK's assistant-message type doesn't expose, so we ask
// for the URLs inside the text where we can actually read them.
const nativeSearchPrompt = `You are a web research assistant. Answer the query using ONLY the web
results provided to you. Reply with a short factual summary followed by a
"Sources:" list of the URLs you used. If the results don't answer the query,
say so plainly. No preamble, no follow-up questions.`

func (NativeWebSearch) Spec() components.ChatFunctionTool {
	return defineTool("openrouter_web_search",
		"Search the web via OpenRouter's built-in search and get a summarized answer with source URLs. Prefer this for current events and facts that need citing.",
		`{"type":"object","properties":{"query":{"type":"string","description":"What to search for"}},"required":["query"]}`)
}

func (t NativeWebSearch) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	if t.Client == nil {
		return "", fmt.Errorf("openrouter client not configured")
	}

	maxResults := t.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	plugin := components.WebSearchPlugin{
		ID:         components.WebSearchPluginIDWeb,
		MaxResults: openrouter.Int64(maxResults),
	}
	if t.Engine != "" {
		plugin.Engine = t.Engine.ToPointer()
	}

	res, err := t.Client.Chat.Send(ctx, components.ChatRequest{
		Model: openrouter.String(t.Model),
		Messages: []components.ChatMessages{
			components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Role:    components.ChatSystemMessageRoleSystem,
				Content: components.CreateChatSystemMessageContentStr(nativeSearchPrompt),
			}),
			components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(query),
			}),
		},
		Plugins: []components.ChatRequestPlugin{
			components.CreateChatRequestPluginWeb(plugin),
		},
	}, nil)
	if err != nil {
		return "", err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return "", fmt.Errorf("search returned no choices")
	}

	answer := strings.TrimSpace(nativeSearchText(res.ChatResult.Choices[0].Message))
	if answer == "" {
		return "No results.", nil
	}
	return answer, nil
}

// nativeSearchText pulls the plain text out of the assistant reply. The SDK
// models content as an optional string-or-parts union; the search sub-request
// only ever asks for text, so anything else means an empty answer.
func nativeSearchText(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}
