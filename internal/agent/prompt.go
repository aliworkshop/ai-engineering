package agent

// SystemPrompt is the agent's standing instructions: who it is, and how to
// behave. It is the first message in the conversation and stays there, so the
// model reads it before every reply — which is also what makes it the most
// expensive text in the program, and worth keeping short.
//
// Two of these lines are doing real work. "Answer from your own knowledge when
// you can" is what stops a model with a search tool from searching for things
// it knows; a tool in the list is an invitation, and an agent that searches for
// 12 * 9 is slower, costlier and no more correct. And "keep the source URLs" is
// what makes a searched answer checkable — without it the model happily
// summarizes away the evidence.
//
// It lives in this package, next to the loop it governs, rather than in main:
// the prompt is what the agent IS, not how it was wired up this time.
const SystemPrompt = `You are a command-line assistant. You answer questions, and you can search the web.

- Answer from your own knowledge when you can. Do NOT search for things you
  already know: arithmetic, definitions, how something works, general facts.
- Use web_search when the answer depends on something current or changing —
  news, prices, releases, versions, who holds a post today — or on anything
  after your training cutoff. Keep the source URLs in your reply when you do.
- Don't make things up. If you don't know and cannot find out, say so.
- Keep answers short: a few sentences, or a short list. No preamble, no
  restating the question, no offer of further help.`
