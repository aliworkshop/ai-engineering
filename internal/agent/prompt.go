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
2. If it needs a data lookup or any arithmetic, use runCode (tools.getCharges
   and tools.searchKnowledgeBase are available inside it).
3. Draft a reply with draftReply, then send it with sendReply.
Work through every item, then briefly summarize what you did.`

// SampleTask is the canned workload: three items, as the workshop has it.
//
// Item 1 is the one runCode exists for. It asks for something the old tools
// could only fake: look through a customer's charges, work out WHICH one is
// the duplicate, and work out the exact refund in dollars from amounts stored
// in cents. Done by chatting that is several tool calls and some arithmetic in
// the model's head, which is the kind of arithmetic models get quietly wrong.
// Done with runCode it is one small program.
//
// Three items rather than one because of memory. One item is about 400 tokens
// of turns, just under the compaction budget, so whether it compacted depended
// on how chatty the model felt that run. Three are well over it, every time,
// and the facts the summary has to carry — which item, which draft, what was
// sent — are what three items have plenty of.
//
// Run it with `go run . -sample`.
const SampleTask = `Handle these work items:
- item-1 (customer_message): "I was charged twice - please find the duplicate and refund the exact amount." (customer: cus_88121)
- item-2 (bug_report): "The export button fails on Safari."
- item-3 (sales_request): "Can you send pricing for 50 seats?"`
