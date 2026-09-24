package agent

// SystemPrompt is the agent's standing instructions: who it is, and how to
// work. It is the first message in the conversation and stays there, so the
// model reads it before every reply — which is also what makes it the most
// expensive text in the program, and worth keeping short.
//
// It lives in this package, next to the loop it governs, rather than in main:
// the prompt is what the agent IS, not how it was wired up this time.
//
// Read what it asks for, then read tools.SendReply. The last step of the
// pipeline emails a customer, and there is not one word here — or anywhere
// else yet — about checking with a human first. The prompt is the only thing
// standing between a model and an irreversible action, and a prompt is not a
// control.
const SystemPrompt = `You are a support triage agent.
For each work item the user gives you:
1. Classify it with classifyItem.
2. Search the knowledge base with searchKnowledgeBase if it helps.
3. Draft a reply with draftReply, then send it with sendReply.
Work through every item, then briefly summarize what you did.`

// SampleTask is the canned workload: three items that between them exercise
// every branch — one the knowledge base answers, one it answers with a known
// bug, and one it has a price for. Run it with `go run . -sample`.
const SampleTask = `Handle these work items:
- item-1 (customer_message): "I was charged twice and need help."
- item-2 (bug_report): "The export button fails on Safari."
- item-3 (sales_request): "Can you send pricing for 50 seats?"`
