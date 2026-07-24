package tools

import (
	"net/http"
	"os"
	"time"
)

// Default builds the standard toolset, wiring the human-in-the-loop approver
// into every dangerous tool. This is the single place that decides which tools
// the agent has and which of them are gated.
func Default(approver Approver) *Registry {
	client := &http.Client{Timeout: 15 * time.Second}

	return NewRegistry(
		// read-only — no approval
		ReadFile{},
		WebSearch{HTTP: client, TavilyKey: os.Getenv("TAVILY_API_KEY")},
		GetWeather{HTTP: client},

		// dangerous — approval required
		WriteFile{Approver: approver},
		EditFile{Approver: approver},
		DeleteFile{Approver: approver},
		RunCommand{Approver: approver},
	)
}
