package triage

import (
	"regexp"
	"strings"
)

// Item is one piece of work. The JSON tags matter: this struct is handed to the
// model as state, and the questions refer to `item` and its fields by name.
type Item struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// itemLine matches one line of a task: `- item-1 (bug_report): "..."`.
var itemLine = regexp.MustCompile(`-\s*(item-\d+)\s*\(([^)]*)\):\s*"([^"]*)"`)

// ParseItems pulls the work items out of a task.
//
// Plain code. No model, no tool, no request — the work items are already
// structured, and asking a model to read a format a regexp can read is how you
// pay for a token to do a string's job.
//
// It also has to be DETERMINISTIC, because it runs outside a step: the loop
// calls it on every replay and must get back the same items in the same order,
// or the triage steps that follow it will not line up with their checkpoints.
//
// A task that names no items becomes one item holding the whole thing, so the
// agent still works when someone types a sentence at it.
func ParseItems(task string) []Item {
	matches := itemLine.FindAllStringSubmatch(task, -1)
	items := make([]Item, 0, len(matches))
	for _, m := range matches {
		items = append(items, Item{ID: m[1], Kind: m[2], Text: m[3]})
	}
	if len(items) == 0 {
		return []Item{{ID: "item-1", Kind: "message", Text: strings.TrimSpace(task)}}
	}
	return items
}

// ByID indexes items for the lookups the loop does per tool call.
func ByID(items []Item) map[string]Item {
	out := make(map[string]Item, len(items))
	for _, item := range items {
		out[item.ID] = item
	}
	return out
}
