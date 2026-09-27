package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// The crash-test dummy's toolbox: fake but realistic support-triage tools.
//
// The point of this set is the shape of it. Three tools are harmless —
// classifying an item, reading the knowledge base, writing a draft — and they
// run the instant the model asks, which is exactly right for work that changes
// nothing. sendReply is not harmless. It "really emails the customer", it
// cannot be recalled, and today it runs with NO mediation: no policy, no
// approval, no record that it happened.
//
// That recklessness is deliberate. It is the thing the rest of the harness
// exists to fix, and it is worth watching it run unguarded once before
// building the machinery that stops it.
//
// The tool names are the workshop's, in camelCase: they are strings the model
// reads, not Go identifiers, and matching the source material is worth more
// here than matching Go style.

// The advertised names, so nothing has to spell one twice.
const (
	ClassifyTool  = "classifyItem"
	KnowledgeTool = "searchKnowledgeBase"
	DraftTool     = "draftReply"
	SendTool      = "sendReply"
)

// KnowledgeBase is the house answer per topic. Four entries, in memory: the
// point of this scenario is what happens around a lookup, not how good the
// retrieval is.
var KnowledgeBase = map[string]string{
	"billing": "Double charges are usually a duplicate authorization that " +
		"drops off in 3-5 days. If it already settled, refund immediately.",
	"refund": "Refunds post in 5-10 business days. Pro accounts can be expedited.",
	"export": "The Safari export failure is a known bug (TICKET-4412). " +
		"Workaround: use Chrome or the CSV export.",
	"pricing": "Team plans are $20/seat/mo with a volume discount at 25+ seats.",
}

// Categories are the only classifications a work item may get. An enum in the
// schema rather than a free string, so the model cannot invent a fifth.
var Categories = []string{"billing", "technical", "sales", "other"}

// ClassifyItem files a work item under one category.
type ClassifyItem struct{}

func (ClassifyItem) Spec() components.ChatFunctionTool {
	return defineTool(ClassifyTool,
		"Classify a work item into a category.",
		`{"type":"object","properties":{`+
			`"item_id":{"type":"string"},`+
			`"category":{"type":"string","enum":["billing","technical","sales","other"]}},`+
			`"required":["item_id","category"]}`)
}

func (ClassifyItem) Run(_ context.Context, args string) (string, error) {
	var a struct {
		ItemID   string `json:"item_id"`
		Category string `json:"category"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.ItemID == "" || a.Category == "" {
		return "", fmt.Errorf("item_id and category are required")
	}
	return marshal(map[string]any{"ok": true, "item_id": a.ItemID, "category": a.Category})
}

// SearchKnowledgeBase returns the house answers a query touches.
type SearchKnowledgeBase struct{}

func (SearchKnowledgeBase) Spec() components.ChatFunctionTool {
	return defineTool(KnowledgeTool,
		"Search the support knowledge base.",
		`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
}

func (SearchKnowledgeBase) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	return marshal(map[string]any{"articles": searchKB(a.Query)})
}

// searchKB is the whole retrieval: an article matches when the query mentions
// its topic. Substring matching on four keys, and no pretence of being more —
// the interesting failure in this scenario is never the ranking.
//
// A miss returns a sentence rather than an empty list, because a model handed
// [] will quietly invent a policy, and one handed "use your judgment" tends to
// say that it is unsure.
func searchKB(query string) []string {
	q := strings.ToLower(query)

	var topics []string
	for topic := range KnowledgeBase {
		if strings.Contains(q, topic) {
			topics = append(topics, topic)
		}
	}
	// Sorted so the same query gives the same answer every time: an unstable
	// tool result makes a conversation unreproducible for no benefit.
	sort.Strings(topics)

	hits := make([]string, 0, len(topics))
	for _, topic := range topics {
		hits = append(hits, KnowledgeBase[topic])
	}
	if len(hits) == 0 {
		return []string{"No exact match - use your judgment."}
	}
	return hits
}

// DraftReply writes a reply without sending it. Drafting is free, which is the
// entire reason it is a separate tool from sending.
type DraftReply struct{}

func (DraftReply) Spec() components.ChatFunctionTool {
	return defineTool(DraftTool,
		"Write a draft reply for a work item. Does not send anything.",
		`{"type":"object","properties":{`+
			`"item_id":{"type":"string"},`+
			`"message":{"type":"string"}},`+
			`"required":["item_id","message"]}`)
}

func (DraftReply) Run(_ context.Context, args string) (string, error) {
	var a struct {
		ItemID  string `json:"item_id"`
		Message string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.ItemID == "" || strings.TrimSpace(a.Message) == "" {
		return "", fmt.Errorf("item_id and message are required")
	}
	return marshal(map[string]any{"ok": true, "draft_id": "draft-" + a.ItemID})
}

// SendReply emails the customer.
//
// DANGEROUS: irreversible, and with zero confirmation — for now. Nothing asks a
// human, nothing checks a policy, and nothing anywhere records that it ran. The
// model asks, and the mail goes out inside the same millisecond.
//
// Read the Run method below and notice how ordinary it looks. That is the
// point: nothing about a dangerous tool announces itself at the call site, so
// the danger has to be handled by the harness around it rather than by whoever
// is reading the code.
type SendReply struct{}

func (SendReply) Spec() components.ChatFunctionTool {
	return defineTool(SendTool,
		"Send the drafted reply to the customer. This really emails them.",
		`{"type":"object","properties":{`+
			`"item_id":{"type":"string"},`+
			`"draft_id":{"type":"string"}},`+
			`"required":["item_id","draft_id"]}`)
}

func (SendReply) Run(_ context.Context, args string) (string, error) {
	var a struct {
		ItemID  string `json:"item_id"`
		DraftID string `json:"draft_id"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.ItemID == "" || a.DraftID == "" {
		return "", fmt.Errorf("item_id and draft_id are required")
	}
	return marshal(map[string]any{"sent": true, "item_id": a.ItemID, "draft_id": a.DraftID})
}

// Args is the shape the support tools' arguments share. Not every field is set
// on every call — draftReply sends a message, sendReply a draft_id — and that
// is the point of having one struct: the harness around these tools needs to
// know which item a call is about without caring which tool it was.
//
// It lives here because this is where the schemas are declared. A caller that
// parsed these arguments for itself would be a second place that has to be
// edited when a schema changes, and the one that gets forgotten.
type Args struct {
	ItemID  string `json:"item_id"`
	Message string `json:"message"`
	DraftID string `json:"draft_id"`
	Query   string `json:"query"`
}

// ParseArgs reads a model's tool arguments. Anything malformed comes back
// zeroed rather than as an error: the caller is looking for an item id, and
// "there isn't one" is an answer it can act on.
func ParseArgs(args string) Args {
	var a Args
	_ = decode(args, &a)
	return a
}

// marshal renders a tool result. Tool results are JSON because the model reads
// them: a structured result is one it can quote a field out of, where prose is
// one it has to interpret.
func marshal(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
