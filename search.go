package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// The one tool: search the web through OpenRouter itself, rather than through a
// third-party search API.
//
// OpenRouter does not expose search as its own endpoint — it models search as a
// *plugin on a chat request*. So the tool sends the query as a one-off
// completion with the "web" plugin attached; OpenRouter runs the search, injects
// the hits into that request's prompt, and the model writes the findings back.
// What we get is therefore an already-summarized answer with source URLs rather
// than raw ranked results, at the cost of one extra model call.
//
// OPENROUTER_API_KEY is the only key you need: searches bill to the same
// account as the model calls.

// SearchTool is the name the model calls, and the name dispatch switches on.
const SearchTool = "web_search"

// SearchModel writes up what the plugin found. Any model works — the plugin
// does the searching — so a small, cheap one is the sensible choice.
const SearchModel = "openai/gpt-4o-mini"

// maxResults caps how many hits OpenRouter feeds into the prompt. Enough to
// cover the usual "what's the current X?" question without paying for a page
// of context that nothing reads.
const maxResults = 5

// searchPrompt shapes the sub-request's answer into something the outer loop
// can quote: facts plus their sources, no conversational padding.
//
// The citation instruction is load-bearing. The chat API returns the plugin's
// citations as annotations that the SDK's assistant message does not expose, so
// we ask for the URLs inside the text, where we can actually read them.
const searchPrompt = `You are a web research assistant. Answer the query using ONLY the web
results provided to you. Reply with a short factual summary followed by a
"Sources:" list of the URLs you used. If the results don't answer the query,
say so plainly. No preamble, no follow-up questions.`

// searchSpec is what the model is told about the tool: a name, a sentence on
// when to reach for it, and a JSON Schema for its arguments. This description
// is the whole of the model's knowledge about the tool, so it is part of the
// program, not a comment.
func searchSpec() components.ChatFunctionTool {
	description := "Search the web and get a summarized answer with source URLs. " +
		"Use it for current events, prices, releases, and anything else you might be out of date on."

	var params map[string]any
	// A compile-time constant schema, so a parse failure is a programming error.
	if err := json.Unmarshal([]byte(
		`{"type":"object","properties":{"query":{"type":"string","description":"What to search for"}},"required":["query"]}`,
	), &params); err != nil {
		panic("bad tool schema: " + err.Error())
	}

	return components.CreateChatFunctionToolChatFunctionToolFunction(components.ChatFunctionToolFunction{
		Type: components.ChatFunctionToolTypeFunction,
		Function: components.ChatFunctionToolFunctionFunction{
			Name:        SearchTool,
			Description: &description,
			Parameters:  params,
		},
	})
}

// webSearch runs the tool. args is the JSON the model produced for the schema
// above — it is model output, so nothing in it is trusted to be well formed.
func webSearch(ctx context.Context, client *openrouter.OpenRouter, args string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("bad arguments %q: %w", args, err)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model: openrouter.String(SearchModel),
		Messages: []components.ChatMessages{
			components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Role:    components.ChatSystemMessageRoleSystem,
				Content: components.CreateChatSystemMessageContentStr(searchPrompt),
			}),
			components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(query),
			}),
		},
		// The plugin is the whole trick: same endpoint, one extra field.
		Plugins: []components.ChatRequestPlugin{
			components.CreateChatRequestPluginWeb(components.WebSearchPlugin{
				ID:         components.WebSearchPluginIDWeb,
				MaxResults: openrouter.Int64(maxResults),
			}),
		},
	}, nil)
	if err != nil {
		return "", err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return "", fmt.Errorf("search returned no choices")
	}

	answer := strings.TrimSpace(text(res.ChatResult.Choices[0].Message))
	if answer == "" {
		return "No results.", nil
	}
	return answer, nil
}
