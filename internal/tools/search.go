package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// WebSearch looks facts up on the web. Read-only, so no approval is needed.
// It uses Tavily when TavilyKey is set (real ranked results) and otherwise
// falls back to DuckDuckGo's keyless Instant Answer API.
type WebSearch struct {
	HTTP      *http.Client
	TavilyKey string
}

func (WebSearch) Spec() components.ChatFunctionTool {
	return defineTool("web_search", "Search the web for current or external facts.",
		`{"type":"object","properties":{"query":{"type":"string","description":"What to search for"}},"required":["query"]}`)
}

func (t WebSearch) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if t.TavilyKey != "" {
		return t.searchTavily(ctx, a.Query)
	}
	return t.searchDuckDuckGo(ctx, a.Query)
}

func (t WebSearch) searchTavily(ctx context.Context, query string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":        t.TavilyKey,
		"query":          query,
		"max_results":    5,
		"include_answer": true,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}

	var b strings.Builder
	if out.Answer != "" {
		fmt.Fprintf(&b, "Answer: %s\n\n", out.Answer)
	}
	for _, r := range out.Results {
		fmt.Fprintf(&b, "- %s (%s)\n  %s\n", r.Title, r.URL, r.Content)
	}
	if b.Len() == 0 {
		return "No results.", nil
	}
	return b.String(), nil
}

func (t WebSearch) searchDuckDuckGo(ctx context.Context, query string) (string, error) {
	endpoint := "https://api.duckduckgo.com/?q=" + url.QueryEscape(query) + "&format=json&no_html=1&skip_disambig=1"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out struct {
		Answer        string `json:"Answer"`
		AbstractText  string `json:"AbstractText"`
		AbstractURL   string `json:"AbstractURL"`
		RelatedTopics []struct {
			Text string `json:"Text"`
		} `json:"RelatedTopics"`
	}
	json.Unmarshal(raw, &out)

	var b strings.Builder
	if out.Answer != "" {
		fmt.Fprintln(&b, out.Answer)
	}
	if out.AbstractText != "" {
		fmt.Fprintf(&b, "%s (%s)\n", out.AbstractText, out.AbstractURL)
	}
	for i, topic := range out.RelatedTopics {
		if i >= 5 {
			break
		}
		if topic.Text != "" {
			fmt.Fprintf(&b, "- %s\n", topic.Text)
		}
	}
	if b.Len() == 0 {
		return "No instant answer found. (DuckDuckGo's free API is limited; set TAVILY_API_KEY for full web search.)", nil
	}
	return b.String(), nil
}
